# Integrate Ramet with your environment

Ramet replaces the workspace allocation and persistence part of your harness.
You keep your agent framework, evaluator, and sandbox runtime. The main question
is whether that runtime can attach a directory on the Ramet worker's filesystem
and reliably stop everything writing to it.

For SkyRL, follow the [environment integration guide](skyrl-integration.md),
including a runnable `BaseTextEnv` adapter and a GPU-free smoke test.

## Framework integration paths

Choose the integration point where your framework runs tools or allocates a
workspace. The paths below are implementation guidance based on the frameworks'
documented extension points, not claims of bundled plugins or tested compatibility.
The SkyRL example has separate executable evidence.

| Framework | Where Ramet fits | Integration status |
| --- | --- | --- |
| LangChain / LangGraph | Acquire an execution in the trusted harness; bind filesystem/shell tools to its sandbox. Record a published snapshot ID with thread/job state and restore it before resumed tools run. | Custom tool/runtime integration; no dedicated plugin tested. |
| CrewAI | Wrap workspace-using custom tools; assign each independent attempt its own execution and sandbox. Keep the execution for tools that intentionally share one attempt. | Custom tool/runtime integration; no dedicated plugin tested. |
| AutoGen | Connect the code executor's workspace binding to the acquired directory, then stop the executor before reset, promotion, or release. | Custom executor integration; its container lifecycle needs lease supervision. |
| SkyRL | Implement `BaseTextEnv.init`, `step`, and `close` around acquisition, tools, and release. | [Runnable same-host Docker example](skyrl-integration.md); distributed training and crash supervision remain application work. |

LangGraph's checkpointers persist graph state; saving a path string there does
not capture that directory's bytes. Keep the framework's persistence and save a
Ramet snapshot ID after explicit publication. Coordinating those two records is
an application recovery protocol, not an atomic transaction supplied by Ramet.

These integration points follow the official documentation for
[LangChain tools](https://docs.langchain.com/oss/python/langchain/tools),
[LangGraph persistence](https://docs.langchain.com/oss/python/langgraph/persistence),
[CrewAI custom tools](https://docs.crewai.com/en/learn/create-custom-tools), and
[AutoGen's Docker executor](https://microsoft.github.io/autogen/stable/reference/python/autogen_ext.code_executors.docker.html),
reviewed on 2026-10-12. With AutoGen, explicitly account for `work_dir` / `bind_dir`
and local-daemon path resolution; passing a worker path alone does not implement
lease-loss shutdown. For all frameworks, keep control-plane credentials and
lease handles in the trusted harness, outside model-visible tool arguments.

## Scope the work

These are implementation scopes, not elapsed-time estimates. A working local
example is smaller than an integration that recovers from runtime and controller
failures.

| Starting point | What you change | What you still need to build |
| --- | --- | --- |
| Trusted scripts on the worker host | Acquire a workspace; use it as `cwd`; promote or release after each attempt. | Application selection and snapshot-ID storage. `execution.run()` is available for trusted foreground commands. |
| Offline Docker commands on the same host | Use the supplied `DockerRuntime.run()` inside an execution context. | Image preparation, selection logic, and external orphan cleanup after harness crashes. No custom mount/command adapter is needed. |
| Existing container runtime with directory mounts | Replace workspace allocation, pass the execution path to the container launcher, and wrap teardown. | A runtime adapter for mount permissions, process stop, lease-loss shutdown, and cleanup recovery. |
| Existing VM runtime | Make the worker directory available in the guest and connect the same lifecycle. | Guest filesystem attachment and detach semantics, plus the runtime adapter above. Ramet does not provide a VM adapter. |
| Remote sandbox service with no worker-path attachment | First establish how the sandbox accesses the worker's directory. | A deployment or filesystem integration with the service. An SDK URL change is insufficient; no generic hosted-sandbox adapter is included. |

Networked agents and long-running sandbox sessions also need a custom runtime
integration. The supplied Docker adapter creates a container for each command
and disables its network. Your controller can call a model service and use the
adapter to execute offline tools, but the adapter cannot run an agent that needs
network access inside its container unchanged.

## Change three places in your harness

| Existing call site | With Ramet | Owner |
| --- | --- | --- |
| Allocate or copy the attempt's working directory | Create/reuse a pool and acquire an execution from a prepared snapshot. | SDK and harness |
| Launch the attempt | Attach `execution.path` as the runtime's workspace and use its in-sandbox path for tool calls. Renew the lease while running. | Runtime adapter |
| Finish the attempt | Stop writers, detach the workspace, promote selected state, and release/discard the execution. Store saved snapshot IDs in your application's job record. | Adapter and harness |

Keep prompts, tool definitions, evaluation criteria, and task scheduling in your
framework. Change tool paths if the workspace mount differs from the old working
directory. Decide whether runtime images contain dependencies or the workspace
supplies them; files outside the workspace are not part of its snapshot.

For the included adapter, the complete command integration is:

```python
from ramet import DockerRuntime, Ramet

ramet = Ramet()
runtime = DockerRuntime("python:3.12-slim-bookworm")
base = ramet.snapshot(source="/absolute/path/on/worker/project")

with ramet.execution_pool(base, concurrency=4) as pool:
    with pool.acquire() as execution:
        result = runtime.run(execution, ["python", "-m", "unittest"], timeout=60)
        # run() has removed its container before returning.
        if result.returncode == 0:
            saved = execution.promote()
            print(saved.id)  # Store this in your application's job record.
```

This assumes a running worker, an installed SDK, and a pre-pulled image. See
[SDK setup and Docker requirements](../sdk/python/README.md#docker-runtime).
For parallel attempts, schedule one such acquisition per task; the pool limits
workspace capacity but does not schedule agents itself.

## Build a custom runtime adapter

Implement these responsibilities around your existing runtime's API:

| Responsibility | Adapter behavior |
| --- | --- |
| Attach | Mount only the acquired workspace. Map UID/GID and permissions, set the working directory, and exclude worker credentials and private storage. |
| Run | Start the agent or tools with your resource/network policy; associate runtime resources with the execution ID so a supervisor can find them. |
| Maintain ownership | Renew the lease during execution and stop the workload on renewal failure or expiry. A watchdog must also act when a renewal request stalls. |
| Stop and detach | Terminate all writers, including child/background processes, and detach the mount before destructive lifecycle operations. Confirm completion through the runtime. |
| Recover | Reconcile partially created resources and failed teardown; an external supervisor handles harness crashes. Keep failed-cleanup workspaces out of normal reuse. |

There is currently **no public runtime registration or cleanup-hook API**.
`DockerRuntime` uses internal SDK hooks to block lifecycle operations while its
cleanup is pending. Do not depend on these private fields for your adapter;
your integration must enforce equivalent ordering. `execution.stop_runtime()`
does not discover or terminate arbitrary external processes.

The following is an **application adapter sketch**, not an additional Ramet
API. You implement `runner.run()` and `runner.stop_and_confirm()` using your
runtime. `run()` must supervise ownership using `renew_lease`, including a
watchdog bounded by the configured lease lifetime. `stop_and_confirm()` must
handle partial startup and raise if it cannot prove writers have stopped and
the workspace is detached.

```python
def run_attempt(pool, runner):
    execution = pool.acquire()
    clean = False
    try:
        try:
            result = runner.run(
                execution_id=execution.id,
                workspace=execution.path,
                renew_lease=execution.heartbeat,
            )
        finally:
            runner.stop_and_confirm(execution.id)
            clean = True
        return execution.promote() if result.selected else None
    finally:
        if clean:
            execution.release()
        # Otherwise propagate the failure; the supervisor must finish cleanup.
        # Do not reset/release/discard or close this pool to bypass that failure.
```

This sketch deliberately uses explicit lease renewal through the adapter.
With `with pool.acquire()`, the SDK renews in the background but also attempts
release on context exit. That is convenient for the included Docker adapter;
a custom adapter must first ensure that a cleanup failure cannot lead to
release. Do not wrap the sketch in automatic pool teardown that bypasses the
failed-cleanup path.

## Failure handling is part of the adapter

An execution lease fences API calls; it does not kill a process with an open
file descriptor. Worker expiry cleanup is not a substitute for runtime shutdown.
After lease loss, stop the workload and obtain a fresh execution for any retry.

On timeout or evaluation failure, stop and detach before saving diagnostic
state or releasing. If teardown fails, propagate the error and let your
supervisor retry cleanup. Do not treat a timeout response from a runtime's
create/stop API as proof that nothing was created or everything stopped.

If a Ramet mutation times out, its outcome may be unknown. Avoid blind retries;
reconcile snapshots, handles, and runtime resources before continuing. After
worker restart, recreate pools from saved snapshot IDs; active executions and
leases are not recovered.

A deployed integration also needs worker placement, authentication, storage
configuration, and supervision. Start with one worker beside one runtime.
Adding PostgreSQL and S3 changes durability for published snapshots; it does
not distribute active directories or schedule work across workers. See
[operations](operations.md) and the [security boundary](../SECURITY.md).

## Check that the integration is ready

Exercise these cases with your actual runtime before relying on it:

- Two concurrent attempts start from the same base and cannot see each other's edits.
- A selected result can be promoted, restored in a new pool, and evaluated again.
- Releasing a modified attempt gives the next attempt a clean base.
- A command timeout stops all writers before release; no background child survives.
- A lost lease or stalled renewal stops the workload without reusing its handle.
- Failed runtime creation and failed cleanup leave no silently running orphan; cleanup failure blocks your harness's release path.
- Killing the harness triggers external orphan cleanup; restarting the worker invalidates old handles.
- Runtime mounts expose only the workspace, and the agent cannot access worker storage or credentials.

The [best-of-N example](../examples/best_of_n/README.md) demonstrates the normal
Docker workflow. The [SDK tests](../sdk/python/tests) cover its command and
lease lifecycle; they do not certify your custom runtime integration.

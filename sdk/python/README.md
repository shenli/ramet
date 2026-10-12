# Ramet Python SDK

Add isolated, resettable workspaces to your agent or evaluation harness.
The SDK calls a Ramet worker; your framework runs the agent and your runtime
provides process isolation. Python 3.10+, with no Python runtime dependencies.

Start here:

- [Install and connect](#install-and-connect)
- [Create, save, and restore a workspace](#create-save-and-restore-a-workspace)
- [Run commands in Docker](#docker-runtime)
- [Connect your own framework or sandbox](#connect-your-own-framework-or-sandbox)
- [Choose what to persist](#choose-what-to-persist)
- [Preview and reconstruct saved state](#preview-and-reconstruct-saved-state)
- [Run parallel attempts](#run-parallel-attempts)
- [Lifecycle reference](#lifecycle-reference)
- [Handle failures](#handle-failures)
- [Warm pools and manage caches](#warm-pools-and-manage-caches)
- [Environment callbacks](#environment-callbacks)

## Install and connect

From a checkout of this repository:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python
```

Start a worker with the repository's [quick start](../../README.md#quick-start),
or connect to an existing worker:

```python
from ramet import Ramet

ramet = Ramet()  # RAMET_URL, or http://127.0.0.1:7433; RAMET_TOKEN if set.
```

You can also pass `url=`, `token=`, and `timeout=` (HTTP timeout in seconds).
A gateway token selects the tenant; the client does not send a tenant ID.
Keep worker credentials in the harness, outside the agent workspace.

**Paths are on the worker.** A remote client can manage snapshots and leases,
but `execution.path` is not downloaded to the client. The examples that access
files directly assume the SDK runs on the same host as the worker.

## Create, save, and restore a workspace

Replace the source path with a project on the worker, outside its data directory.
Stop source writers while capturing it. The worker must be running.

```python
from ramet import Ramet

ramet = Ramet()
base = ramet.snapshot(source="/absolute/path/to/project")

with ramet.execution_pool(base, concurrency=4) as pool:
    with pool.acquire() as execution:
        # Replace this write with your agent's tools or runtime integration.
        (execution.path / "result.txt").write_text("selected result\n")
        saved = execution.promote()
    # Context exit resets and releases this workspace; source is unchanged.

print("Save this snapshot ID in your application:", saved.id)
with ramet.execution_pool(saved.id, concurrency=1) as pool:
    with pool.acquire() as restored:
        print((restored.path / "result.txt").read_text())
```

The result is `selected result`. Snapshot IDs survive worker restart; execution
paths, pools, and leases do not. `promote()` saves state without ending the
execution. A new pool based on that snapshot restores the saved state.

To import a repository instead, use
`ramet.snapshot(repository="https://host/org/repo.git", revision="<commit>")`.
An explicit revision is required; pin a commit for reproducible attempts.
Git import enables shared base-pack storage while preserving local-only Git
state. Hydrate submodules and LFS content yourself when needed.

## Docker runtime

This is the included runtime adapter. It runs foreground commands in a fresh
container per call. Use it for offline tools and tests; it does not supply a
long-running agent session or network access to model APIs.

Run the SDK and worker where a local Docker daemon can mount the returned
workspace paths. Use the copy/reflink backend, and pull the image first:

```sh
docker pull python:3.12-slim-bookworm
```

With a worker running, replace the source path with your Python project:

```python
from ramet import DockerRuntime, Ramet

ramet = Ramet()
runtime = DockerRuntime("python:3.12-slim-bookworm", cpus=1, memory_mb=256)
base = ramet.snapshot(source="/absolute/path/to/project")

with ramet.execution_pool(base, concurrency=4) as pool:
    with pool.acquire() as execution:
        result = runtime.run(execution, ["python", "-m", "unittest"], timeout=60)
        print(result.stdout, result.stderr)
        # The container has been removed before run() returns.
        if result.returncode == 0:
            saved = execution.promote()
            print("Saved:", saved.id)
```

`run()` returns `subprocess.CompletedProcess[str]`; use `check=True` to raise
on a nonzero exit. Arguments must be a list, not a shell command string.
The execution context renews its lease. The adapter handles command timeout,
observed renewal failures, acknowledged lease expiry, and container cleanup.
See [failure handling](#handle-failures) for cleanup failures and caller crashes.

The container mounts only `execution.path`, at `/workspace`. It uses the
caller's UID/GID, a read-only root, a 64 MiB `/tmp`, no network or Linux
capabilities, and CPU/memory/process limits (`pids=128` by default). Image
entrypoints are overridden. Files outside `/workspace` are not captured.
Client, worker, and daemon must resolve the same path; remote Docker daemons
and FUSE mount propagation are unsupported. Combined stdout/stderr is limited
to 1 MiB per command; set `max_output_bytes` on `DockerRuntime` to adjust it.
Exceeding the limit raises an error and stops the container before reuse.
The adapter is not containment for
hostile code or kernel exploits; see the [security boundary](../../SECURITY.md).

For a complete workflow that evaluates several fixes and restores the winner,
see the [best-of-N coding example](../../examples/best_of_n/README.md).

## Connect your own framework or sandbox

You keep your framework's agent loop, tools, and evaluator. Change the code
that allocates each attempt's working directory, launches its runtime, and
saves or cleans up the result. Ramet supplies snapshots, pools, workspace paths,
reset, and leases.

| Your environment | Integration work |
| --- | --- |
| Same-host trusted scripts | Wrap each attempt in a pool acquisition and use its path as the working directory. |
| Included Docker adapter | Replace command launch with `runtime.run(execution, args)`; choose when to promote. Add external orphan cleanup for harness crashes. |
| Existing container or VM runtime | Implement workspace attachment, process lifecycle, lease-loss shutdown, and cleanup recovery. VM filesystem attachment is runtime-specific. |
| Hosted sandbox without worker-path attachment | Resolve filesystem access first; configuring the SDK URL alone cannot integrate it. |

The [integration guide](../../docs/integration.md) details the call sites,
adapter responsibilities, failure cases, and acceptance checks. There is no
public custom-runtime registration API today: the built-in Docker adapter's
cleanup hooks are internal. A custom runtime must enforce the lifecycle
contract in its own integration.

For **trusted same-host experiments**, `execution.run(["python", "script.py"],
timeout=60)` runs a local subprocess and returns a `CompletedProcess`.
It does not create a sandbox. Commands must not escape their process group
or write elsewhere on the host. Use your sandbox for agent-generated code.

## Choose what to persist

Attach path rules to the base snapshot. They apply when an execution is
**promoted**, not during initial capture or `checkpoint()`:

```python
policy = {"rules": [
    {"path": "node_modules", "class": "rebuildable", "recipe": "npm ci"},
    {"path": "scratch", "class": "ephemeral"},
    {"path": "fixtures", "class": "shared_immutable"},
]}
base = ramet.snapshot(source="/absolute/path/to/project", policy=policy)
```

| Class | Promotion behavior |
| --- | --- |
| `durable` (default) | Preserve current state. |
| `shared_immutable` | Retain base state; fail if matching content was added, edited, or removed. |
| `rebuildable` | Omit bytes and retain the required recipe. Your runtime executes the recipe after restore. |
| `ephemeral` | Omit matching state, retaining parents needed by included descendants. |

Rules are relative path prefixes; the longest matching prefix wins. Unknown
paths are durable, and `.git` cannot be excluded. Initial snapshots and
checkpoints capture all supported files, including paths marked ephemeral.
See [persistence and restore fidelity](../../docs/operations.md#persistence-policy).

## Preview and reconstruct saved state

Inspect a quiesced execution before promotion:

```python
preview = execution.preview_promotion()
print(preview["retained_bytes"], preview["omitted_bytes"])
if not preview["valid"]:
    raise RuntimeError(preview["violations"])
saved = execution.promote()
print(saved.manifest["save"]["mode"], saved.manifest["save"]["excluded"])
plan = ramet.restore_plan(saved)
```

Preview lists per-path decisions and immutable-policy violations. Byte totals
are logical sizes; optimized Git packs are excluded. It publishes no state,
though reads can hydrate a lazy workspace. Promotion rescans rather than
trusting an earlier preview. `manifest.save.mode` distinguishes `base`,
`checkpoint`, and `promotion`; the report records exclusions and rebuild steps.

Rebuildable rules can include `inputs` (retained regular-file paths) and
`runtime` (your expected image/toolchain identity). Promotion records input
hashes. The returned step ID binds the recipe, inputs, and runtime so your
application can approve an exact reconstruction contract.

Inside an acquired execution context, call
`execution.rebuild(approved_handlers, verify=check_application_ready)`.
`approved_handlers` maps reviewed step IDs to callbacks receiving `(execution,
step)`. Each callback runs a known operation through your runtime and stops its
writers before returning. The SDK does not interpret recipe strings or choose
an image. It checks all approvals before running steps, rechecks declared input
hashes, and requires the verifier to return exactly `True` before returning
`ready: true`. Failures raise `RebuildError` with a structured `report` and the
underlying exception. There are no automatic retries or persistent readiness
flags. `execution.restore_plan()` only checks inputs; it does not rebuild.

Start with the [complete runnable example](../../examples/persistence/README.md).
See [persistence policies](../../docs/persistence.md) for callback contracts,
legacy snapshots, full-checkpoint inheritance, and Python/Node policy examples.

## Run parallel attempts

Pool concurrency is a capacity limit; it does not launch tasks. Your harness
schedules them. This example uses the `ramet`, `runtime`, and `base` from the
Docker example:

```python
from concurrent.futures import ThreadPoolExecutor

with ramet.execution_pool(base, concurrency=4) as pool:
    def evaluate(attempt):
        with pool.acquire() as execution:
            result = runtime.run(execution, ["python", "-m", "unittest"], timeout=60)
            return {"attempt": attempt, "passed": result.returncode == 0}

    with ThreadPoolExecutor(max_workers=4) as executor:
        results = list(executor.map(evaluate, range(8)))
```

Separate executions can run concurrently; one handle serializes its lifecycle
operations. `pool.acquire(count=N)` reserves a batch atomically and returns a
list. `N` must fit pool capacity and worker quotas. Batch handles do not start
heartbeats automatically: enter each execution context promptly or renew each
handle yourself, and release/discard every handle before closing the pool.

## Lifecycle reference

| Operation | Result and use |
| --- | --- |
| `ramet.snapshot(...)` | Capture a directory or import Git; return a `Snapshot` with `id` and `manifest`. |
| `ramet.snapshots()` | List published snapshots in this store/tenant. |
| `ramet.execution_pool(base, concurrency=8, lease_seconds=900)` | Create a pool from a `Snapshot` or snapshot ID. |
| `pool.acquire()` | Return a leased `Execution` with worker-local `path` and `id`. |
| `execution.heartbeat()` | Renew ownership; required outside an execution context. |
| `execution.reset()` | Return to the pool's base; updates this handle's path and generation. |
| `execution.fork()` | Copy current state into another leased execution in the same pool; consumes capacity. |
| `execution.verify()` | Compare against the pool's base. Your intended edits can produce differences. |
| `execution.checkpoint()` | Save all supported current files, without promotion exclusions. |
| `execution.preview_promotion()` | Inspect policy decisions and logical bytes without publication. |
| `ramet.restore_plan(snapshot)` | Read the snapshot's reconstruction contract. |
| `execution.restore_plan()` | Also check current declared input bytes. |
| `execution.rebuild(handlers, verify=...)` | Run approved callbacks and verify readiness; raises `RebuildError` on failure. |
| `execution.promote()` | Save current state according to the base's persistence policy. |
| `execution.release()` | Reset and release; also called by execution context exit, including on exceptions. |
| `execution.discard()` | Remove this execution without saving it. |
| `execution.stop_runtime()` | Retry cleanup registered by the built-in Docker adapter. |
| `pool.close()` | Remove the pool and idle trees; also called by pool context exit. Release active executions first. |

Stop external writers before fork, capture, reset, release, or discard; detach
sandbox mounts before replacing or releasing a workspace. After reset, use the
updated path and remount it. A fork's reset returns to the original **pool base**,
not the fork point. Neither checkpoint nor promotion ends the active execution.
Lease duration defaults to 900 seconds and supports 1–86,400 seconds.

## Recoverable mutations

`ramet.new_operation()` issues a one-hour ID scoped to the current worker API
session. Persist it before a mutation and pass `operation_id=id` to `snapshot`,
`execution_pool`, `pool.acquire`, or execution `reset`, `fork`, `checkpoint`,
`promote`, `release`, and `discard`. `ramet.operation(id)` reads its receipt.
After response loss, the identical call with the same ID returns the original
result without repeating its effect. Different request bytes are rejected.

This is opt-in, with no automatic retry. Receipts are bounded and in memory;
restart or expiry invalidates the ID and requires application reconciliation.
A recorded execution result can have an expired lease. See the
[complete recovery example and limits](../../docs/agents.md#recover-an-uncertain-mutation).
`ramet.capabilities()` reports backend, versions, and receipt limits.

## Handle failures

| Failure | What your integration should do |
| --- | --- |
| HTTP error | Catch `RametError`; inspect `status`, `code`, `request_id`, `retry_after`, and its message. A stale/expired handle cannot be reused. |
| Connection error or request timeout | Treat a mutation's outcome as unknown. Do not blindly retry snapshot, promotion, acquisition, or other mutations. Use a pre-issued operation ID for recoverable calls; see [operation recovery](../../docs/agents.md#recover-an-uncertain-mutation). Without one, stop and reconcile through your application. |
| Workload timeout or nonzero exit | Decide whether to discard or save diagnostic state after writers stop. The Docker adapter removes its container even when the command raises. |
| Docker cleanup fails | Lifecycle changes remain blocked. Restore daemon access and call `execution.stop_runtime()` before retrying release. Do not bypass cleanup through another client. |
| Lease renewal fails or expires | Stop the workload. The Docker adapter observes these conditions; generic execution contexts renew leases but do not terminate your custom runtime. |
| Harness crashes | An external runtime supervisor must terminate orphaned workloads before their workspaces can be reclaimed. Docker containers carry the label `ramet.runtime=python`. |
| Worker restarts | Recreate pools from saved snapshot IDs. Old execution paths and leases are invalid. |

Unkeyed transport exceptions retain their standard-library types. Keyed mutations
raise `OutcomeUnknownError`, which carries the original `operation_id`. Mutations are not
automatically retried; background heartbeat renewal can retry transient errors.
The HTTP timeout and the runtime command timeout are separate settings.
See [runtime integration](../../docs/integration.md#failure-handling-is-part-of-the-adapter)
for custom-runtime cleanup requirements.

## Warm pools and manage caches

These controls are optional optimizations after the integration works:

```python
ramet.prefetch(base)  # Download and verify snapshot content on the worker.
with ramet.execution_pool(base, concurrency=4) as pool:
    print(pool.warm(4))       # Newly prepared and currently idle counts.
    print(ramet.locality(base))
    with pool.acquire() as execution:
        print(execution.path)

print(ramet.cache_status())
ramet.trim_idle()             # Remove idle trees; active executions remain.
# Remote storage mode only:
# ramet.trim_cache(target_bytes=0)
```

Warm prepares up to the requested idle count; it does not maintain that target
automatically. Configured idle-count and idle-age limits govern retention.
`locality()` is a residency hint, not an integrity check. Prefetch verifies
content but does not pin it against eviction. Remote cache trimming can return
429 if readers pin content. Lazy Linux workspaces hydrate and verify content
on open; `execution.verify()` still reads the full tree.
See [worker cache limits](../../docs/operations.md#cache-and-admission-limits).

## Environment callbacks

`RametEnv(pool, observe, act)` provides `reset()`, `step(action)`, `state()`,
and `close()` for harnesses organized around an environment loop. `observe`
receives the current `Execution`; `act` receives the execution and action.
Both return application-defined values. Use it as a context manager to release
its execution, and keep the pool open until the environment closes.

The callbacks own runtime execution, observations, and rewards. Before resetting
or closing, stop custom runtime writers. This adapter does not implement the
Gymnasium API or supply a scheduler.

For SkyRL-Gym's `init`/`step`/`close` contract, use the
[SkyRL integration guide and runnable environment](../../docs/skyrl-integration.md).
It includes task metadata, registration, Docker execution, and placement and
cancellation requirements for rollout workers.

## Examples and development

- [Best-of-N coding workflow](../../examples/best_of_n/README.md): Docker commands, evaluation, promotion, and restore.
- [Parallel workspace example](../../examples/python/parallel_eval.py): trusted local files and thread scheduling.
- [Integration guide](../../docs/integration.md): plan and validate your runtime adapter.
- [Google AX assessment](../../docs/ax-integration.md): custom-runner opportunities and filesystem integration requirements.
- [Operations](../../docs/operations.md): worker configuration, HTTP API, storage, and recovery.

From the repository root, run `make build`, then
`PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v`.
For Docker lifecycle tests, pre-pull `python:3.12-slim-bookworm` and run
`RAMET_TEST_DOCKER=1 make test-python` (`RAMET_TEST_IMAGE` overrides the image).

Licensed under [Apache-2.0](../../LICENSE).

# SkyRL integration

**Use Ramet underneath a SkyRL environment to give each rollout an isolated
filesystem, then discard it or save selected state.** SkyRL owns generation,
trajectories, rewards, and training. Ramet owns workspace snapshots and leases;
your runtime runs the tools. This fits coding, repository-editing, and other
file-based RL tasks. Pure text/math tasks usually do not need a workspace engine.

The [runnable adapter](../examples/skyrl/ramet_env.py) implements SkyRL-Gym's
`BaseTextEnv` using Ramet's existing Python SDK and same-host Docker adapter.
No Ramet core change or SkyRL fork is needed for this basic lifecycle. It is an
integration template, not a supported distributed training backend.

Interfaces were reviewed against SkyRL commit
[`a1e34dbf21003f66bd7921487fd47db6c154aba2`](https://github.com/NovaSky-AI/SkyRL/tree/a1e34dbf21003f66bd7921487fd47db6c154aba2),
whose `skyrl-gym` package declares version 0.4.0. Pin the revision used by your
training setup; do not assume every SkyRL release has the same generator API.

## What you implement

| SkyRL lifecycle | Ramet integration | Application responsibility |
| --- | --- | --- |
| Prepare tasks | Snapshot each prepared repository once; store its ID with task metadata. | Prepare fixtures, policies, runtime image, and evaluator. |
| `init(prompt)` | Acquire a fresh execution, start lease renewal, and verify the initial files. | Attach its directory to the runtime; reconstruct omitted dependencies if needed. |
| `step(action)` | Run tools against the same execution directory across turns. | Parse actions, produce observations, compute rewards, and decide termination. |
| Finish a selected attempt | Stop writers, preview/promote, and record the returned snapshot ID. | Select results and persist the ID in the training/job record. |
| `close()` | Stop runtime writers, release/reset the execution, then close its owned pool. | Handle cancellation, failed teardown, and orphan cleanup. |

For the included offline Docker runtime, the adapter work is a small environment
class plus registration and dataset metadata. For your own persistent container,
VM, or hosted sandbox, also implement attachment, lease-loss shutdown, and
confirmed teardown; see [runtime integration](integration.md). Ramet does not
reset process memory, databases, browser sessions, simulator state, or RNGs.

## Run the environment without a GPU

Use a Linux host with a local Docker daemon, Go, Python 3.10+, and this checkout.
Install Ramet and the pinned lightweight SkyRL-Gym package in a disposable venv;
the full SkyRL trainer, model server, and Ray cluster are not needed for this test.

```sh
go build -o bin/ramet ./cmd/ramet
python3 -m venv .venv
.venv/bin/python -m pip install ./sdk/python \
  'skyrl-gym @ git+https://github.com/NovaSky-AI/SkyRL.git@a1e34dbf21003f66bd7921487fd47db6c154aba2#subdirectory=skyrl-gym'
docker pull python:3.12-slim-bookworm
./bin/ramet serve --data .test-cache/skyrl-store --listen 127.0.0.1:7444
```

In another terminal, from the same checkout:

```sh
export RAMET_URL=http://127.0.0.1:7444
.venv/bin/python -m unittest discover -s examples/skyrl -p test_smoke.py -v
```

Use a dedicated **copy-backend** worker for this example. DockerRuntime is the
copy-workspace adapter; enabling FUSE requires a custom runtime attachment as
described in the [FUSE guide](fuse.md). The tests create small published snapshots
in that worker's data directory; use a disposable store for testing.

The example task is deliberately small: use tool commands to write the expected
text into `result.txt`. Model actions are JSON, for example:

```json
{"argv":["sh","-c","printf ramet > result.txt"],"finish":true}
```

`argv` runs in Docker at `/workspace`; `finish` triggers a harness-supplied
grader in a separate container. The resulting reward is 1 for matching contents,
otherwise 0. Invalid action syntax yields feedback and consumes a turn.
Infrastructure failures raise exceptions instead of being labeled wrong answers.
Replace this toy task's action parser and grader with your actual tools and
trusted evaluation logic; it is not a coding benchmark or reward design.

Containers have no network and are recreated for each command. Files persist
across steps, but shell state and background services do not. The SDK stops the
container before a tool call returns, so selection/promotion can happen after
`step()` and before `close()`. The model service runs outside these containers.

The [smoke tests](../examples/skyrl/test_smoke.py) exercise actual SkyRL-Gym
registration, two simultaneously active isolated workspaces, reward/termination,
selective promotion and restore, a clean new episode, command timeout, idempotent
close, and the rule that failed cleanup prevents pool deletion. They do not run
GPU training, distributed Ray placement, or certify cancellation recovery.

Validated on 2026-10-12 with the pinned SkyRL-Gym revision, Python 3.10, a Linux
copy worker, and real Docker commands: **4/4 tests passed**, with no runtime
containers left behind. The cleanup-failure test injects a release error to
check ordering; the other lifecycle checks use the real worker and containers.

## Wire it into SkyRL training

Start with SkyRL's
[custom environment entrypoint](https://github.com/NovaSky-AI/SkyRL/blob/a1e34dbf21003f66bd7921487fd47db6c154aba2/examples/train/multiply/main_multiply.py).
Make `ramet_env.py` importable in the Python environment of every rollout process
(for a local experiment, add this checkout's `examples/skyrl` to `PYTHONPATH`).
Use the same Ramet SDK and environment module versions on every node.

Register the environment in the entrypoint task, following SkyRL's example:

```python
from skyrl_gym.envs import register

register(id="ramet_command", entry_point="ramet_env:RametCommandEnv")
```

Registration is process-local. If a custom deployment creates generators in
separate actors/processes, run this registration in those processes too. Do not
only register in a driver that never constructs environments.

Prepare the snapshot on the runtime host with a trusted harness. The source path
is absolute on that worker, outside its store, and has no active writers:

```python
from ramet import Ramet

base = Ramet().snapshot(source="/absolute/path/to/prepared-task", policy={
    "rules": [{"path": "scratch", "class": "ephemeral"}],
})
```

Each dataset row uses SkyRL's `prompt`, `env_class`, `reward_spec`, and
`extra_info` fields. For this example:

```python
row = {
    "prompt": [{"role": "user", "content":
        'Write ramet into result.txt with no newline. Respond only with JSON: '
        '{"argv":["program","arg"],"finish":false}. '
        'Use {"finish":true} when finished.'}],
    "env_class": "ramet_command",
    "reward_spec": {"method": "rule", "ground_truth": "ramet"},
    "extra_info": {"ramet_snapshot_id": base.id},
}
```

The reviewed
[dataset loader](https://github.com/NovaSky-AI/SkyRL/blob/a1e34dbf21003f66bd7921487fd47db6c154aba2/skyrl/train/dataset/dataset.py)
passes the non-prompt fields to environment `extras`; the
[generator](https://github.com/NovaSky-AI/SkyRL/blob/a1e34dbf21003f66bd7921487fd47db6c154aba2/skyrl/train/generators/skyrl_gym_generator.py)
adds `max_turns`. Keep tokens, leases, and worker credentials out of dataset
rows and model observations. Supply `RAMET_URL` and `RAMET_TOKEN` to the trusted
rollout process using your deployment's environment/secret configuration.

Configure SkyRL for multiple tool turns (`generator.max_turns` and
`generator.use_conversation_multi_turn`) using the options supported by your
pinned version. Its `environment.skyrl_gym.max_env_workers` controls the thread
executor for synchronous environment calls. Keep it positive for these blocking
HTTP/Docker calls and size it alongside rollout concurrency. This executor is
not a workspace quota or a node-placement policy.

## Placement, scaling, and failure handling

**Start with one rollout host, one Ramet worker, and one local Docker daemon.**
The returned execution path belongs to that host. An HTTP connection to a worker
on another node does not make its directory available to Docker on this node.
If your Ray rollout process itself runs in a container, it must have the required
host path mapping and local daemon access; Docker's source mount resolves on the
daemon host. Only mount the acquired execution into the agent container.

For multiple rollout hosts, route each environment to its colocated worker and
ensure its selected base is available there. PostgreSQL/S3 can make published
snapshots available to replacement workers; they do not distribute active
execution directories or schedule Ray actors. Persist snapshot IDs alongside
task IDs, image versions, evaluation versions, and seeds for reproducibility.

The template creates a one-slot pool per trajectory to make ownership clear.
That does not bound total trajectories or reuse warm pools across episodes.
For sustained training, a host-local manager should own reusable pools keyed by
base snapshot and policy, bound total active acquisitions, and close pools when
draining. Environment `close()` should release only its own execution in that
design. Do not serialize a live SDK handle, renewal thread, or pool between Ray
actors. Measure reset/acquire latency and capacity on your actual task corpus.

**Cancellation needs additional integration.** In the reviewed generator's
multi-turn path, `env.close()` is called on normal completion, while the outer
`finally` releases the inference session. It does not guarantee environment
cleanup after every generation error or cancellation. For production, wrap the
environment lifecycle in `try/finally` in your custom generator/harness, join
in-flight environment calls before closing, and use an external supervisor for
actor crashes. Cancelling an asyncio task does not stop an already running
thread-executor call. See SkyRL's
[agent integration choices](https://docs.skyrl.ai/docs/tutorials/agent-integration)
for the custom generator extension point.

The SDK renews leases while the environment is alive, and its Docker adapter
stops commands on lease failure/expiry. A crashed harness still needs external
orphan-container cleanup. A lease alone cannot terminate arbitrary processes.
Never delete a pool or recycle its workspace after failed runtime teardown.

The template does not implement a durable supervisor journal or automatic
mutation retries. An HTTP timeout during acquire/promote/release can leave an
unknown outcome; stop the trajectory and reconcile rather than rerunning it
blindly. Add the [operation-ID recovery protocol](agents.md#recover-an-uncertain-mutation)
when building that supervisor. Following a worker restart, reconstruct pools
from saved snapshots rather than resuming old execution handles.

Before training at scale, test actor cancellation during tools, harness death,
worker restart, stalled lease renewal, lost mutation responses, and cleanup
failure with your real deployment. The supplied smoke test establishes the
same-host environment contract, not those distributed failure guarantees.

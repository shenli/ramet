# Google AX integration assessment

**Ramet can be integrated through custom runtime work; the stock AX manifests do
not provide a Ramet workspace backend.** This is a source-based assessment, not
an end-to-end compatibility certification. No AX/Agent Substrate cluster was
used for this assessment.

Reviewed Google AX commit
[`ac2332829f22360ff97b0ba34d94dd0dd782f17e`](https://github.com/google/ax/tree/ac2332829f22360ff97b0ba34d94dd0dd782f17e)
on 2026-10-09. AX is actively evolving, so recheck these interfaces when building
an adapter.

## Where they fit

AX declares and schedules tasks, configures workspaces, runs the task sandbox,
and delegates durable volume suspend/resume to Agent Substrate. Ramet could
supply immutable filesystem bases, isolated attempts, explicit promotion
policies, and reconstruction plans within that lifecycle. Keeping agent work
across suspend/resume is already part of AX's documented `/workspace` contract;
Ramet's added value would be selective state retention and reusable attempt
lifecycles, rather than basic volume persistence.

| Verified AX interface | Integration consequence |
| --- | --- |
| Task `spec.image` can replace the runner image; entrypoint remains `/usr/local/bin/ax-task-runner` | A custom runner is an available extension point. |
| Runner prepares workspace bindings before `/readyz` reports ready | Acquisition and approved reconstruction must complete before advertising readiness. |
| Runner starts the task command in the first workspace directory | The custom runner must supply the acquired workspace path to the command/tool configuration. |
| Substrate supplies a durable `/workspace` volume and restores it on resume | Do not confuse an AX volume path with a directory on an external Ramet worker. |
| Public `runner.Config` exposes `OnCommandExit` | A hook exists, but command exit alone is not proof all writers are stopped. |
| `WorkspaceSpec` contains Git, inline files, MCP and skills fields | There is no Ramet snapshot field, arbitrary host-path volume binding, or pluggable persistence-provider field in the reviewed API. |

Sources: [runner contract](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/docs/runner.md),
[runner implementation](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/runner/runner.go),
and [API schema](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/pkg/apis/v1alpha1/ax.proto).

## Feasible paths

**Trusted proof of concept inside one AX task.** Package a custom runner or
trusted command wrapper with Ramet's Go engine or local worker. Capture an
already prepared workspace, acquire an isolated attempt, and run trusted tools
against that execution path. Keep the Ramet store outside the captured source.
This exercises the lifecycle but does not establish isolation between agent
code and the state engine when both share a sandbox. An ephemeral store also
does not establish cross-task snapshot persistence.

**Integration for untrusted agents.** Run the state engine and its storage
credentials outside the agent's sandbox. This needs either an AX/Substrate
filesystem attachment extension that exposes only the selected worker-local
execution, or an explicit transfer bridge between AX volumes and Ramet workers.
Neither is implemented here. The current Ramet HTTP API returns local paths; it
does not stream a mounted workspace over the network. Merely setting
`RAMET_URL` inside an AX task is insufficient. Transfer-based integration would
also incur copies and would not preserve Ramet's direct local-workspace path.

A complete custom runner/adapter would need to:

1. Translate the task's selected base/snapshot and policy into pool acquisition.
2. Expose the execution directory through the chosen attachment or transfer mechanism.
3. Run approved reconstruction and readiness checks before `/readyz` succeeds.
4. Renew leases and stop writers on lost ownership, including stalled renewals.
5. Quiesce all task writers before preview/promotion; save snapshot IDs in a
   controller-owned job record before releasing the execution.
6. On resume, recreate transient pools/leases from saved snapshot IDs. Do not
   resume stale Ramet paths just because AX restored its volume.
7. Handle SIGTERM, failed publication, failed cleanup, and harness crashes
   without releasing a workspace that still has writers.

The default runner has no configurable pre-workspace hook in the reviewed
`runner.Config`; `OnCommandExit` alone is insufficient. It also keeps the task
inspectable after command exit. A custom runner must account for background
children and any debug/guest-service writers before capturing state. Coordinate
Ramet publication with AX suspend rather than assuming a post-exit callback
provides a filesystem barrier.

## Suggested next validation

First test the custom-runner lifecycle with trusted commands and the
[selective persistence example](../examples/persistence/README.md). Then validate
the chosen sandbox attachment on an actual AX/Substrate cluster: cross-task
isolation, lease loss, suspend/resume, publication failure, and orphan cleanup.
Only after that should the project advertise a supported AX adapter.

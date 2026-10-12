# Integrate Ramet from an agent harness

Ramet manages workspace files. Your harness runs the agent, stops its processes,
and attaches the returned worker-local path to your sandbox. Start with the
[README installation](../README.md#quick-start) and use a worker on the same host
as the runtime that can attach its paths. Remote HTTP access does not transfer
files to a remote sandbox.

## Discover and validate

```sh
./bin/ramet capabilities
./bin/ramet policy-validate --policy examples/policies/python.json --json-errors
```

Capabilities report API/server version, platform, copy/lazy backend, and receipt
limits. The [policy schema](../schemas/policy-v1.schema.json) supports editor and
agent-side validation. `policy-validate` additionally checks duplicate rule
paths and path semantics; promotion checks that recipe inputs actually exist
as retained regular files. Unknown policy fields are errors.

| Environment | Supported attachment |
| --- | --- |
| Same host, trusted development commands | Use `execution.path` directly. |
| Same-host Docker adapter | Copy/reflink workspace bind; adapter manages command teardown. |
| Native Linux FUSE | Use the [FUSE guide](fuse.md); your custom sandbox owns mount attachment and teardown. |
| Remote sandbox without worker-path attachment | Resolve attachment before integrating the SDK. |

## Minimal verified lifecycle

Use this with a running local worker and the installed Python SDK. Replace the
source path; keep it outside the worker data directory and stop source writers
while snapshotting.

```python
from ramet import Ramet

ramet = Ramet()
base = ramet.snapshot(source="/absolute/path/to/project", policy={"rules": [
    {"path": "scratch", "class": "ephemeral"},
]})
with ramet.execution_pool(base, concurrency=1) as pool:
    with pool.acquire() as execution:
        # Your sandbox attaches execution.path and runs its workload here.
        # Stop writers and detach the sandbox before the following calls.
        (execution.path / "result.txt").write_text("selected result\n")
        preview = execution.preview_promotion()
        assert preview["valid"], preview["violations"]
        saved = execution.promote()

with ramet.execution_pool(saved, concurrency=1) as pool:
    with pool.acquire() as restored:
        assert (restored.path / "result.txt").read_text() == "selected result\n"
        assert not (restored.path / "scratch").exists()
        assert restored.verify()["valid"]
```

Save `saved.id` in your application. Published snapshots survive a worker
restart; pools, leases, and active writes do not. Recreate pools from the saved
ID after restart. PostgreSQL/S3 is required to survive loss of the worker disk.
Rebuildable paths need [approved reconstruction and readiness checks](persistence.md),
not just a successful filesystem restore.

## Recover an uncertain mutation

For operations that need response-loss recovery, obtain and persist an operation
ID **before** the request. Python supports `operation_id=` on snapshot, pool
creation, acquisition, reset, fork, checkpoint, promotion, release, and discard.
Go uses `client.WithOperationID(ctx, id)`; the CLI uses `--operation-id`.
The underlying header is `Idempotency-Key`.

```python
from ramet import OutcomeUnknownError

with ramet.execution_pool(base, concurrency=1) as pool:
    with pool.acquire() as execution:
        operation_id = ramet.new_operation()
        # Persist operation_id and the intended request in your harness journal.
        try:
            execution.reset(operation_id=operation_id)
        except OutcomeUnknownError:
            receipt = ramet.operation(operation_id)
            if receipt["state"] != "completed":
                raise  # Keep writers stopped; inspect again after completion.
            execution.reset(operation_id=operation_id)
        assert execution.verify()["valid"]
```

Recover inside the execution/pool context when possible. An unresolved operation
error leaving a context stops registered runtime/heartbeat activity and avoids
hiding the recovery error behind a failed release/close. The supervisor still
owns reconciliation, cleanup, and unregistered custom runtimes. Keep the same
handle/request until recovery; never mutate its generation or launch new writers
while the outcome is unknown.

Receipts last **one hour from ID issuance**, within **one worker API session**.
They are in memory and do not survive restart or worker replacement. Expired IDs
return `operation_expired`; IDs from another session return
`operation_session_changed`. Both prohibit replay. A lost result after that
boundary requires application-level reconciliation of saved snapshots and
runtime cleanup; creating a new ID is not evidence that repeating work is safe.

A receipt records a historical response, not current lease validity or workspace
readiness. Validate a recovered execution before use. Python's keyed reset also
renews the returned lease before resuming it. Stop if that renewal fails.

### Recovery decisions

| Result | Next action |
| --- | --- |
| `completed` receipt | Read the original status/body, or repeat the identical request with the same ID to recover the typed result. Errors are also recorded. |
| `pending` / `operation_pending` | Keep writers stopped; poll the read-only receipt endpoint with bounded backoff. |
| `operation_not_found` | This session has not accepted the ID yet. Retry only the same request and ID; an in-flight original can still arrive. |
| `operation_conflict` | Stop: method, path, or exact request bytes differ. Restore the original request; do not replace its ID to bypass the conflict. |
| `operation_capacity` | No operation was accepted by the receipt layer. Wait for space and retry the same request/ID before expiry. |
| `operation_expired`, `operation_session_changed`, `operation_result_unavailable` | Outcome cannot be recovered through this receipt. Stop and reconcile through the application; never infer that nothing happened. |
| Unkeyed transport failure | Outcome unknown; there is no receipt. Stop writers and use the application recovery procedure. |
| Explicit quota or other server rejection | Inspect code, request ID, and receipt. Do not assume all HTTP failures are safe to repeat as a new operation. |

Limits are 10,000 receipts and 64 MiB of response storage per worker API session,
with 8 MiB reserved per in-flight keyed operation. Capacity rejection happens
before its effect. A response larger than 8 MiB produces an unknown-result
receipt and cannot be reexecuted under that ID. Expired completed receipts are
reclaimed on the next keyed request; IDs themselves remain expired. Operation
IDs are optional; normal requests retain their existing behavior and are not
automatically retried.

Receipts containing execution handles include lease tokens. Treat them like the
handles: private harness state, never agent prompts or public logs. Tenant
credentials select the receipt namespace. Keep worker credentials outside the
agent's workspace and use the harness to make control-plane calls.

## Troubleshoot without guessing

API errors contain `code`, `error`, and `request_id`. Both SDKs expose request ID
and `Retry-After`; Python raises `OutcomeUnknownError` for transport/response
failures on keyed mutations. CLI commands support `--json-errors`, writing one
JSON diagnostic to stderr and returning nonzero. Successful CLI output remains
JSON where the command returns data.

Correlate request IDs with worker logs. Use `health` for readiness, `doctor` for
store checks, and `verify` for workspace content. Follow the
[integration acceptance checks](integration.md#check-that-the-integration-is-ready)
before running a custom sandbox integration.

# Recover tool progress together with workspace files

A tool that produces files has two outputs: its workspace changes and the result
returned to the harness. Publish those outputs together before advancing the
harness's durable checkpoint. Ramet's durable publications provide this boundary;
the harness still owns workflow state, scheduling, authorization, and external
effects.

## The recovery order

1. Persist the session/branch scope, step identity, base snapshot, environment,
   tool version, and inputs in the harness checkpoint.
2. Resolve the step's publication ID. If it exists, reuse its saved files and
   result without running the tool again.
3. Otherwise acquire a new execution from that base, prepare dependencies, run
   the tool, and stop all writers.
4. Publish the workspace and JSON result under the same stable publication ID.
5. Only after publication succeeds, return that record as the node's output so
   the harness can advance its own durable checkpoint.

The workspace snapshot and tool result share one content-addressed manifest.
There is no separate receipt write that could be lost after snapshot commit.
The harness checkpoint is a later, separate transaction. A crash between those
transactions leaves a recoverable publication, not a harness checkpoint pointing
at unpublished files.

## Python integration

```python
from ramet import WorkspaceStep

step = WorkspaceStep(
    scope=state["session_branch"],     # Persist before execution; new scope for a fork.
    step="generate-report",            # Include an occurrence ID for repeated calls.
    base=state["snapshot"],
    environment=runtime.dependency_environment(),
    inputs={"topic": state["topic"]},
    tool_version="report-tool-v1",
)
record = step.run(ramet, generate_report, prepare=prepare_document_dependencies)
# Return record as this node's output. Do not advance the workflow before this.
```

The returned JSON-compatible record contains `snapshot`, `publication_id`,
`environment`, and `result`. The tool callback receives an `Execution` and
returns a JSON value of at most 64 KiB, including its encoded representation
in the manifest. Store large outputs in workspace files
and return their paths and hashes. The optional preparation callback can use
[dependency compatibility checks](session-recovery.md).

`WorkspaceStep` derives a stable publication ID from scope, step, and base.
A separate request digest binds the environment, inputs, tool version, and save
mode. Changing any of these under an already published ID fails closed.
Use JSON-compatible inputs with string object keys and update the tool version
when its behavior changes. Intentional exploration needs a new branch scope.

`run()` first resolves publication. It does not automatically retry failed
commands or uncertain HTTP mutations. Calling it again after a restart resolves
the same ID before creating another execution. A missing publication does not
prove an old publisher stopped: the caller must fence the old harness/runtime
and serialize execution of each session before attempting recovery.

A failed tool may have run before publication. Uncommitted workspace-only work
may therefore run again from the last saved base. This helper cannot safely
repeat payments, messages, PR creation, or arbitrary external effects. Keep those
in your workflow engine's idempotency/reconciliation mechanism.

## Lower-level API

- `execution.publish(id, request_digest, result, mode="checkpoint")` captures a
  quiesced execution. `mode="promotion"` applies its persistence policy instead.
- `ramet.publication(id)` returns the saved `Snapshot`, or `None` when absent.
- Go exposes `Pool.Publish(ctx, execution, engine.PublishOptions{...})` and
  `Client.Publication(ctx, id)`. Embedded users call `Store.GetPublication`.
- HTTP publishes through
  `POST /v1/pools/{pool}/executions/{execution}/publish`, with lease, generation,
  mode, and a publication object containing id, request_digest, and result.
  Lookup uses `GET /v1/publications/{id}`.

Both IDs must be lowercase SHA-256 hex strings. Use this durable ID as the logical
publication key; the existing `Idempotency-Key` mechanism remains scoped to a
worker session and is not needed here. On an uncertain response the Python and Go
clients raise `PublicationUnknownError`, carrying the durable lookup ID.

A matching repeated publish returns the original snapshot even if the supplied
execution has since changed; it does not capture those newer files. A different
request digest, parent snapshot, or save mode returns a conflict. This is
idempotent publication, not exactly-once tool execution. Credentials still scope
lookup to the same tenant/store.

| Failure boundary | Recovery |
| --- | --- |
| Before publication | No completed result; fence old writers, restore the committed base, and rerun workspace-only work |
| During content upload | No visible publication until all content and metadata are committed |
| Metadata committed but response lost | Resolve the same publication ID; never switch to a new ID |
| Publication completed, harness checkpoint missing | Reuse the saved result and snapshot when the node is retried |
| Harness checkpoint completed | Restore its referenced snapshot and run the next node |
| Provider unreachable or ownership lost | Fail closed; an outage must not be interpreted as an absent publication |

## Persistence and operational limits

Local mode retains publications across restart when the storage directory
survives. PostgreSQL/S3 mode retains them across replacement with an empty worker
directory. Workers still have exclusive namespace ownership; this is not a
concurrent multi-worker controller.

Publications use manifest version 5. Readers accept versions 1–5; older binaries
cannot read the new manifests. The first publication lookup rebuilds an in-memory
ID index by scanning authoritative manifests; subsequent lookups read and verify
the indexed manifest. Index rebuilding is proportional to retained snapshot
count. Publication operations are serialized per worker namespace.

Published snapshots are pinned against deletion through the engine/API so a
forgotten key cannot later execute as new work. There is currently no expiry or
release API for those pins. Parent lineage is also retained, and remote object GC
remains deferred. Do not enable unlimited per-tool checkpointing without a
retention budget; use disposable namespaces for the example. Direct edits/deletes
in the underlying metadata store are unsupported.

The helper is framework-neutral. The [LangGraph example](../examples/langgraph_recovery/README.md)
uses an existing durable graph checkpointer; it does not replace it. Neither
publication nor graph recovery captures process memory or system packages
outside the workspace. Runtime attachment and cleanup follow the existing
[integration contract](integration.md).

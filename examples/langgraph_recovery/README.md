# LangGraph document recovery with crash injection

A reference integration for a sequential document-producing graph. LangGraph
keeps graph progress in a SQLite checkpointer, while Ramet publishes each tool's
files and JSON result together. The same pattern can wrap another harness.

The two tools create a DOCX report and then continue editing it. They run in
fresh containers using the existing [document session image](../document_session/README.md).
The runtime image is pinned, dependencies are installed offline into the
workspace, and matching dependencies are reused.

The runner exercises five cases:

| Case | Expected draft executions | Expected continuation executions |
| --- | --- | --- |
| Completed graph, worker replaced, graph resumed | 1 | 1 |
| Harness killed after tool completion but before publication | 2 | 1 |
| Harness killed after publication but before graph output | 1 | 1 |
| Publication reply lost | 1 | 1 |
| Harness killed after the durable graph checkpoint | 1 | 1 |

It also kills the first worker without graceful shutdown. Provider mode gives
the replacement worker an empty data directory; local mode retains its durable
store. The harness's SQLite database survives independently. This is a
single-host replacement test, not proof of multi-host placement or host power-loss
durability.

## Run

Requires Go, Python 3.10+, and local Docker. From the repository root:

```sh
make build
python3 -m venv .test-cache/langgraph-venv
.test-cache/langgraph-venv/bin/pip install -r examples/langgraph_recovery/requirements.txt
docker build -t ramet-document-session:local examples/document_session
SESSION_IMAGE=$(docker image inspect ramet-document-session:local --format '{{.Id}}')
PYTHONPATH=sdk/python .test-cache/langgraph-venv/bin/python \
  examples/langgraph_recovery/run.py --image "$SESSION_IMAGE" \
  --output .test-cache/langgraph-local
```

Use a new output directory for each run. It retains private worker/harness logs,
graph state, tool call counts, and `results.json`. Sandbox commands need no
network or model API key. Installing the example dependencies and building the
image require network access.

For durable providers, start disposable PostgreSQL/MinIO services, create the
test bucket, and set credentials as described in the document session example.
Then add:

```sh
--worker-config examples/document_session/providers.test.yaml
```

The test publishes permanently pinned snapshots in its provider namespace.
Do not run against production services. This example uses the Python SDK's
`WorkspaceStep`; refer to [tool recovery](../../docs/tool-recovery.md) for the
publication contract and its limits.

## Adapt to your harness

Use a persistent checkpointer and synchronous checkpoint durability. In
LangGraph, resume unfinished work with `graph.invoke(None, config,
durability="sync")`; supplying new input starts a new run. Preserve the scope,
step occurrence, base snapshot, environment, tool inputs and tool version across
retries. Serialize each session's execution, and fence the old runtime before
a replacement starts.

Return the published record from a node only after `WorkspaceStep.run` succeeds.
Keep the framework checkpoint as the authority for the current task position.
On intentional time-travel/fork, assign a new branch scope if tools should run
again instead of reusing an earlier publication. Parallel branches need distinct
scopes and an application-defined join; this example does not merge files.

Tool callbacks may affect only their private workspaces. Side effects outside
those workspaces require the harness's existing idempotency and reconciliation.
The external attempts log is a test witness for detecting duplicate tool runs,
not part of the product's recovery mechanism.

## Customer validation still needed

This example is not evidence about a customer's S3 mount or sandbox integration.
For a pilot, use the same document edits and durability boundaries with both
systems. Record uploaded bytes and request counts, checkpoint latency,
cold/warm restore latency, restored file hashes, repeated work, retained storage
after the run, and adapter code/setup effort. Capture compression and cache
settings and run repeated trials before comparing results.

For a genuine two-host test, keep harness state durable outside either worker,
fence the first host, place the pinned image on the second, and restore from the
same PostgreSQL/S3 namespace into an empty directory. Never share a live SQLite
checkpointer over a network filesystem as a substitute for a durable harness DB.

See the [validation record](../../docs/testing/2026-11-10-tool-recovery.md) for
what has actually run. CI definitions alone are not validation results.

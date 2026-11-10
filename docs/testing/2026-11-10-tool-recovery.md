# Tool recovery validation — 2026-11-10

Local and durable-provider recovery checks passed for the changes in this
commit, based on `9fa4aaa3aae64c9147fcd1a59c286b667e7a78ba`.
Ramet remains an alpha; these results establish the tested boundaries, not
production scale or arbitrary external-effect recovery.

## Environment

- macOS arm64, Go 1.26.9, Python 3.13.7, local Docker.
- LangGraph 1.2.20, langgraph-checkpoint 4.3.1, SQLite checkpointer 3.1.3.
  The example pins these tested versions.
- Document runtime image:
  `sha256:b28cb5c668fb0e8112a3d3fd20465daa388df2cc562f283474229b7cae4ee3d4`.
- Disposable PostgreSQL and MinIO from `compose.integration.yaml`.

## Executed checks

- `make fmt` and `make check`: passed. This includes Go race tests, worker
  compilation, Go vet, formatting, and the real-worker Python suite.
  Python ran 37 tests: 34 passed and three opt-in Docker tests skipped.
- The 22 standard-library Python unit tests also passed independently.
- `make test-integration`: passed against real PostgreSQL/MinIO, including
  `TestPublicationReconcilesRemoteCommitWithoutReply`, which commits metadata
  and injects a lost reply before reopening with an empty worker directory.
- The LangGraph runner passed all five cases in both storage modes below.
  These runs used real document-generation commands in Docker.
- Python syntax, changed Markdown relative links, and `git diff --check` passed.
  The public-text scan found only existing hash fragments in lockfiles and
  historical benchmark evidence. No private planning documents are included.

The initial isolated candidate environment lacked Go, Docker, and LangGraph.
Those preliminary verification blockers were resolved by testing in the normal
workspace before this commit.

## Crash and reply-loss results

Each row passed once in local mode and once with PostgreSQL/S3. Both modes
forcibly killed the first worker. Local mode retained its durable directory;
provider mode started the replacement worker with an empty directory.
The harness SQLite checkpointer survived independently.

| Boundary | Draft executions | Continuation executions | Result |
| --- | --- | --- | --- |
| Completed graph, worker replaced, graph resumed | 1 | 1 | Passed in both modes |
| Harness killed after tool completion, before publication | 2 | 1 | Passed in both modes |
| Harness killed after publication, before graph output | 1 | 1 | Passed in both modes |
| Publication reply lost | 1 | 1 | Passed in both modes |
| Harness killed after durable graph checkpoint | 1 | 1 | Passed in both modes |

The runner verified invocation counts, snapshot lineage, restored artifact
hashes, and successful document editing after restoration. A tool that finishes
before publication can repeat; completed publications are reused.

Reproduction commands, from the repository root after installing the example
requirements and preparing the document image:

```sh
make check
docker compose -p ramet-phase1-test -f compose.integration.yaml up -d --wait
make test-integration
PYTHONPATH=sdk/python .test-cache/langgraph-venv/bin/python \
  examples/langgraph_recovery/run.py --image "$SESSION_IMAGE" \
  --output .test-cache/langgraph-local

# Set disposable provider credentials as documented in the example.
PYTHONPATH=sdk/python .test-cache/langgraph-venv/bin/python \
  examples/langgraph_recovery/run.py --image "$SESSION_IMAGE" \
  --worker-config examples/document_session/providers.test.yaml \
  --output .test-cache/langgraph-remote
docker compose -p ramet-phase1-test -f compose.integration.yaml down
```

See the [example](../../examples/langgraph_recovery/README.md) for complete setup.
Use new output directories; raw logs, graph databases, and results remain in
ignored test directories.

## Limits and remaining validation

- Linux FUSE tests were not run on this macOS host. The integration suite's lazy
  backend cases skipped; this is not evidence of FUSE behavior for this change.
- The generated artifact benchmark skipped because fixture paths were not set.
  Existing artifact benchmark results are separate evidence.
- The three optional standalone Docker lifecycle tests skipped in `make check`;
  the LangGraph runs exercised the actual Docker adapter separately.
- Hosted CI is configured but its result is not claimed here.
- These are single-host process/worker replacement tests, not two-host placement
  or host power-loss tests. SQLite state was retained locally.
- No customer S3-mount comparison, cost/latency advantage, external-effect
  exactly-once guarantee, or unlimited retention claim is established.
  Publication snapshots are pinned without an expiry/release API.

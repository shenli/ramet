# Implementation status

The design document describes the intended product across several phases. This
file records the shipped code's narrower support boundary. The current version
is a **Phase 2 development alpha**, suitable for evaluating lifecycle semantics,
tenant-scoped worker deployments and trusted runtime integrations.

For the current implementation, see [architecture](architecture.md),
[design decisions](design.md), and the [developer guide](development.md).

## Implemented

- Go library, CLI, single-process worker, versioned HTTP API, Go HTTP client.
- Python SDK with context-managed pools/executions and a generic environment adapter.
- Same-host Docker adapter and a deterministic best-of-N coding workflow with
  container tests, winner patch export, verified recycle, and promoted-state resume.
- Directory capture and commit-pinned Git import with independent local Git state.
- SHA-256 content store, immutable versioned manifests, sync-before-publication,
  complete checkpoints and selective promotion with deterministic retry IDs.
- APFS clonefile and Linux FICLONE execution materialization, explicit copy fallback.
- Atomic batch acquisition, worker-wide quotas, expiring renewable leases, and
  local forks without durable writes.
- PostgreSQL schema migrations and exclusive namespace ownership; immutable
  S3/MinIO content published before metadata; verified empty-cache recovery.
- Reset/recycle with replacement directories, deep verification, lease/generation
  fencing, quarantine-on-failure, and cleanup.
- Explicit persistence classes with conservative durable defaults and recorded recipes.
- Local restart recovery, exclusive store ownership, snapshot deletion guards,
  grace-period mark/sweep GC, doctor, health, metrics, and drain.
- Docker/Compose packaging, Linux/macOS CI, correctness tests, race tests,
  fuzz target, repeatable benchmark and soak workload.
- Apache-2.0 licensing and contributor/security documentation.

Phase 0 and Phase 1 acceptance gates are implemented. Linux validation covers
3,000 attempts over three cohorts of 1,000 live slots with no contamination or
durable loop writes. This small-base result does not establish the broader V1
performance targets. See [validation](validation.md) for the evidence and its limits.

## Phase 2

Implemented: provenance-aware Git object partitioning and offline reconstruction;
explicit warm/prefetch/locality APIs; pinned LRU remote caches and idle expiry;
disk headroom; Linux FUSE file-open hydration; tenant credential routing with
separate stores/source allowlists/budgets; bounded HTTP latency metrics and
structured error correlation; reader-safe cache trimming and collection.

The [Phase 2 validation](validation.md#phase-2-release-validation) records acceptance results.
The [validation report](validation.md) records measured Linux duration and resource
trends. Resource-isolated workload containers are exercised separately from
worker API quotas. The shared gateway does not impose hard per-tenant process
CPU/memory limits; use separate worker processes for that requirement.

## Next milestones

| Milestone | Remaining work / acceptance evidence |
| --- | --- |
| Git extensions | Automated dependency hydration, incremental local-pack chains and additional workspace profiles |
| Scale qualification | Hours-long large-base/fan-out evidence, physical shared-extent accounting, automatic warm replenishment and placement scheduling |
| Distributed workers | Worker registration, cross-host routing, cross-worker live-lease coordination and failover |
| Production workspaces | Workspace API, durability journal/writeback, runtime mount integration, recovery SLOs |
| Environment state | Snapshot coordinator and barriers, structured state, clock/identity/secret rebinding, external-binding contracts |

Process-memory snapshots, transparent external-side-effect rollback, multi-writer
coherence, a scheduler, and a full RL framework remain deferred per the design.

## Performance boundaries

The local engine shares file extents where the filesystem supports clones. Each
execution still has its own directory entries and inodes. The default copy backend reads every file on acquire/reset. The optional Linux
lazy backend prepares metadata first and verifies bytes on open; Git metadata
is eager. Explicit verify and fork still read the tree. A reset rebuilds the tree; it is not an O(1) overlay swap.
Fallback copies use space proportional to base size × execution count.

The repository includes measured small-workload results and tools for larger
experiments. A 10 GB / 1,000-execution result, production SLOs and hours-long
stability claims require further measurements and optimizations. The project
does not assert those targets have already been achieved.

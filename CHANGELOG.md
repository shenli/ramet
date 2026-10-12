# Changelog

## Unreleased

- Bounded Docker command output with cleanup before workspace reuse on overflow.
- SkyRL-Gym environment example and integration guide; framework integration
  paths and updated workspace architecture overview.
- Strict CLI persistence-policy decoding, local policy validation, JSON Schema,
  and required-argument checks before network access.
- Optional operation IDs, bounded session-scoped receipts, and safe replay of
  identical mutations after response loss; expired/restarted sessions fail closed.
- Request IDs and retry metadata in SDK errors, consistent API routing errors,
  CLI JSON diagnostics, capabilities discovery, and an agent integration guide.

- Linux FUSE setup guide and configuration, mapped-write/reset isolation tests,
  and Python SDK lifecycle coverage against the lazy backend in CI.

- Advisory promotion previews with path decisions, logical-byte estimates, and
  immutable/input-policy violations, exposed through CLI, HTTP, Go, and Python.
- Manifest v3 records save mode, exclusions, and reconstruction steps with input
  hashes and caller-declared runtime identity; v1/v2 remain readable.
- Python reconstruction runs only approved step-ID callbacks and reports
  readiness after caller verification; failures preserve a structured report.
- Selective persistence example, opt-in Python/Node policies, and a source-based
  Google AX integration assessment.

## 0.3.0-alpha.1

Phase 2 adds bounded preparation and storage behavior for long-running parallel
agent workloads. This is an alpha API; the module root is no longer a Go package.
Embedded callers import `github.com/shenli/ramet/engine`.
The worker HTTP implementation lives in `internal/server`; public Go integration
uses `engine`, `client`, and `storage`.

- Best-of-N coding example with three deterministic fixes, containerized tests,
  winner patch export, clean recycle, and verified promoted-state restore.
- Python Docker adapter with bounded resources, lease monitoring, timeout
  teardown, and lifecycle blocking when container cleanup fails.
- Reproducible full-copy / Git-worktree / Ramet comparison with explicit
  differences in Git isolation and verification work.
- Reference MinIO services build pinned upstream source, replacing unavailable
  registry images. CI covers the Docker workflow and scheduled security scans.

- Shared durable Git base packs and local-only object packs preserve logical Git
  state across repacks and offline restores. New manifests use version 2.
- Explicit warm pools, prefetch and locality hints; bounded idle slots and expiry.
- Remote-cache LRU eviction with reader pins, coalesced fetches and disk headroom.
- Opt-in Linux FUSE hydration on first open, with writable private inodes,
  namespace verification, failure/retry and mount recovery.
- Credential-routed tenant stores, source/Git allowlists, independent capacity
  budgets and request admission limits.
- HTTP latency histograms, request IDs, tenant-aware error logs, hydration/cache
  metrics and background maintenance error reporting.
- Real-provider pressure tests, Linux runtime-isolation checks and a reproducible
  sustained workload recording memory, descriptors, disk and latency.
- Reject non-UTF-8 manifest paths and text before publication to prevent JSON
  serialization from silently changing acknowledged filenames or symlink targets.
- Parse nanosecond lease timestamps on Python 3.10 so automatic heartbeats
  continue renewing short-lived execution leases.
- Updated architecture, design, developer and operations guides; cross-platform
  release archives and Python SDK 0.3.0a1. Patched Go 1.26.9 minimum.

Copy/reflink materialization remains the default. Remote object deletion,
distributed scheduling, hard per-tenant process isolation inside the shared
gateway, durable live workspaces and process-memory snapshots are not included.

## 0.2 development alpha

Phase 1 introduced durable PostgreSQL/S3 publication, worker replacement,
independent writable executions, renewable leases, batch acquisition, local
forks, reset/recycle, checkpoints, policy promotion and Apache-2.0 licensing.

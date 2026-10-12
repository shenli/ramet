# Validation evidence

Phase 1 validated on 2026-10-08 with Go 1.26.8 on macOS arm64 and
Linux amd64 (Ubuntu 22.04, kernel 6.8, ext4).
The current implementation and support boundary are listed in
[implementation status](implementation-status.md). The product design includes
future stages beyond the current worker implementation.

## Prelaunch FUSE review (2026-10-12)

Native Linux amd64 validation used Go 1.26.9 and Python 3.10 on a dedicated test host:

- All five native FUSE lifecycle tests passed ten consecutive times with the
  race detector. Coverage includes mapped writes, truncate extension, restored
  permissions, isolation, reset, crash recovery, Git, and policy previews.
  The subprocess helper skips in the parent process and runs in crash recovery.
- The Python suite passed 15 tests against a real `lazy` worker. Its two Docker
  tests were excluded: the included adapter supports copy/reflink workspaces.
- All nine real PostgreSQL/MinIO integration tests passed, including injected
  hydration failures on both copy and lazy backends and worker replacement.
- The complete Go race suite passed on Linux and `make check` passed on macOS.
  README and SDK lifecycle examples ran successfully.
- Four platform archives and the Python wheel/sdist built successfully;
  contents include licenses and exclude private planning/test data.
- The dependency scan found zero reachable vulnerabilities. An all-ref secret
  scan found no leaks in the 24 commits present at review time.

The previously cancelled hosted provider job had incomplete logs, so these
native results do not establish its cause. CI now runs verbose, time-bounded
FUSE tests and the Python lifecycle against FUSE. Require a successful hosted
run on the candidate commit before publication. The earlier F3 pressure timeout
remains open; see the [hardening report](testing/2026-10-09-hardening.md).

See [Linux FUSE setup](fuse.md) for reproducible commands and runtime limitations.

## AX regression evidence

See the [2026-10-12 AX checks](testing/2026-10-12-ax.md) for policy validation,
response-loss recovery, and error/discovery improvements.

## Evidence

| Requirement | Executable evidence | Result |
| --- | --- | --- |
| Execution isolation and stale-handle protection | `TestIsolationResetAndLeaseFencing`, filesystem independent-inode test | Passed with race detector |
| Local fork without durable writes | `TestForkIsLocalAndIsolated` | Passed |
| Verified reuse under concurrency | `TestConcurrentRecycleSoak`, Python parallel integration | Passed |
| Corruption quarantine | `TestCorruptBaseQuarantinesReset` | Passed |
| Snapshot durability ordering and deduplication | Sync-before-publication implementation; concurrent capture/GC and failed-publication tests | Tests passed; ordering also reviewed in source |
| Selective persistence and resume | Promotion/restart test, durable-descendant/deletion test, Python promotion/fork test | Passed |
| Local Git history retention | `TestGitBaseAndLocalCommitSurvive` | Passed |
| Path and symlink containment | Manifest validation tests, chained-link test, `FuzzManifestPaths` | Passed; short fuzz run exceeded two million inputs |
| Recovery and exclusive store ownership | `TestStoreLockFormatAndRecovery` | Passed |
| Capacity, cancellation, drain, closure | Capacity/drain test, close-wakes-acquire test, HTTP deadline test | Passed |
| API authentication and input bounds | HTTP token, invalid JSON, body-size tests | Passed |
| Local developer workflow | CLI demo test; `make check` | Passed |
| Distributable Python SDK | Built wheel/sdist; installed wheel into a fresh virtual environment and ran all five real-worker integration tests | Passed; LICENSE, NOTICE, and type marker included |
| Linux runtime | Full race suite on a dedicated Linux host, compiled and run with Go 1.26.8 | Passed; ext4 copy fallback |
| PostgreSQL/MinIO providers | `scripts/test-integration.sh` | All five tests passed: conformance, empty-cache worker replacement, upload/publication failure, connection termination, newer schema rejection |
| Batch and lease recovery | `engine/leases_test.go`, Python batch/automatic-heartbeat test | Passed; quota reclamation, expiry, fencing, cancellation and rollback |
| Remote fan-out | `scripts/run-phase1-gate.sh`, checked-in Linux JSON | 3,000 successes; three cohorts of 1,000 live slots, 32 mutation/recycle threads; no failures or durable loop writes |
| Reference Compose deployment | Built and started `compose.remote.yaml` on a dedicated Linux host; authenticated API lifecycle | Healthy; snapshot, batch, verification, heartbeat, recycle and doctor passed |
| Dependency security | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | Zero reachable vulnerabilities; one advisory in an unused required-module package |
| Repeatable benchmark | Checked-in JSON and source metadata under `docs/benchmarks/` | 1,000 successful COW cycles, zero verification/quarantine failures, zero durable loop writes |
| Open-source packaging | Apache-2.0 LICENSE, copyright NOTICE, third-party notices, contributor/security docs | Included |
| Internal planning remains private | Git ignore, Docker build-context exclusion, CI guard; wheel content inspection | Internal marketing document excluded |

`make check` includes all Go tests with the race detector, a fresh CLI build,
Python integration against that worker, `go vet`, and a formatting check.
The Python wheel was additionally tested independently of `PYTHONPATH` to catch
packaging omissions. `git diff --check` passed.

## Scope of the evidence

The Linux tests ran in `~/work/ramet-phase1` using isolated test PostgreSQL and
MinIO services. The host's existing service databases were not used. The
reference Compose deployment was separately built and exercised, including its
non-root image, bucket initialization, and health check. Test containers were
stopped after validation; their data and logs are retained for inspection.

The earlier macOS COW benchmark records the local-alpha baseline. The new Linux
[fan-out result](benchmarks/phase1-linux-fanout.json) uses copies on ext4 and a
small Git fixture. [Provenance](benchmarks/phase1-linux-fanout.metadata.json)
identifies the tested source and host. Zero loop remote-write counters exclude
initial snapshot publication.

CI is configured for Linux/macOS with Go 1.26/1.27, real provider tests, Python
packaging, and container smoke tests. Hosted CI was not run during this initial
Phase 1 validation; check the candidate commit's Actions results before release.
The standalone installed SDK wheel is tested independently of `PYTHONPATH`.

This validation does not establish power-loss durability of a particular disk,
hours-long stability, physical shared-extent amplification, a 10 GB /
1,000-execution target, production SLOs, or distributed worker correctness.
These remain subsequent workload-specific acceptance gates.

## Phase 2 release validation

Phase 2 validation ran on 2026-10-09 UTC (2026-10-08 local time), on macOS arm64
and Linux amd64/ext4, in `~/work/ramet-phase2`. The dedicated integration
PostgreSQL/MinIO services were stopped after validation; test state/logs remain
under that checkout. Existing service databases were not used.

| Gate | Result |
| --- | --- |
| `make check` with Go 1.26.9 | Passed: race suite, all seven Python real-worker tests, vet and formatting |
| Linux Go 1.26.9 race suite and vet | Passed |
| Real PostgreSQL/MinIO suite | All eight top-level tests passed, including both copy/lazy cache-pressure subtests and empty-cache optimized Git reconstruction |
| Native Linux FUSE | File operations, concurrent hydration, retry after provider outage, reset, explicit verification, cleanup and killed-worker mount recovery passed |
| Kernel interruption regression | Canceled individual kernel request does not abort inode hydration; passed |
| Git provenance | Local commits, staged/unstaged state, refs/reflogs, dangling objects, repack dedup, offline restore, corrupt packs and forged remote-tracking refs covered |
| Tenant separation | Pool/snapshot access rejection, independent admission limits, source-root escape rejection, real two-tenant provider workload passed |
| Runtime limits | Real Docker workload verified 0.5 CPU, 64 MiB memory, 32 PIDs, network disabled, capabilities dropped, no-new-privileges, read-only root and one execution mount |
| Manifest fuzzing | 10-second run, 3,735,271 inputs, no failure |
| SDK distribution | Wheel/sdist built; fresh installed wheel passed all seven real-worker tests without source `PYTHONPATH`; type marker and licenses checked |
| Binary/container packaging | Linux/macOS amd64/arm64 archives built; non-root Linux image built and completed 20 demo cycles; license/archive contents checked |
| Dependency security | Patched Go 1.26.9 scan: zero reachable vulnerabilities, zero in imported packages; one advisory in an unused required-module package |
| Documentation/privacy | Architecture diagram rendered and inspected; local links and whitespace checked; internal marketing file remains ignored and absent from tracked files/artifacts |

The security scan prompted a toolchain update from 1.26.8 to 1.26.9. The full
race/provider/native FUSE suites and container checks were rerun with 1.26.9.
The long run was already using 1.26.8; its historical result is retained honestly.
A separate [patched-build pressure run](benchmarks/phase2-linux-release-smoke.json)
completed 1,839 cycles in 122.7 seconds with no workload errors. The execution/cache/lazy paths are identical between those runs; source and
binary hashes are embedded in each report. A final Git-only change routes pack
materialization through clone/copy to share extents where supported; Git and
provider regressions and a combined lazy/Git lifecycle test were rerun for that
change. HTTP deadline coverage isolates the intentionally blocked acquire so
normal filesystem operations do not depend on a 100 ms test-only deadline. [Run metadata](benchmarks/phase2-linux.metadata.json)
records toolchain and workload distinctions.

## Sustained pressure result

The [30-minute report](benchmarks/phase2-linux-soak.json) completed **29,476 cycles
in 1,801.3 seconds**, with **zero workload errors** and clean worker shutdown.
Eight threads used two independently scoped tenants: 3,496 copy cycles and
25,980 lazy cycles. Each tenant had a 1 MiB cache for a 4 MiB, 64-file base.
Every cycle read known base bytes and wrote private state. Periodic cycles also
reset, forked or promoted/restored while prefetch, trimming and GC ran concurrently.

| Resource / outcome | Observed |
| --- | --- |
| Process RSS | 23.3–45.5 MiB sampled; 30.7 MiB after pools closed |
| RSS during second half | 34.5–45.3 MiB; no monotonic growth in sampled series |
| Open descriptors | 28–64 sampled; 33 after pools closed |
| OS threads | Increased from 34 to 107 during warm-up; held at 107 throughout the second half |
| Local physical test-tree disk use | Peak 53.7 MiB; 6.1 MiB after pool cleanup; excludes remote provider storage |
| Remote cache limit | Both stayed at or below 1 MiB; 146,780 combined object evictions |
| End-of-run pins / reserved bytes / idle slots | Zero for both tenants |
| Explicit trim pressure | 153 expected pressure responses while readers pinned content; no workload failure |
| Copy cycle p50 / p95 / p99 | 1.601 / 3.276 / 9.840 seconds |
| Lazy cycle p50 / p95 / p99 | 0.031 / 1.614 / 4.950 seconds |
| Cold / warm copy acquire | 1.098 / 0.013 seconds (single samples) |
| Cold / warm lazy acquire | 0.014 / 0.002 seconds (single samples) |

Cycle latency includes occasional fork and durable promotion, not just acquire.
Lazy avoids copying unread files; this workload's access pattern is part of the
result. Both tenants share the host and providers, and additional release checks
ran during the tail. These measurements establish bounded behavior for this
profile, not an isolated backend speed comparison. The samples remain available
in JSON for independent inspection.

This is a 30-minute alpha gate, not hours-long large-base qualification, physical
shared-extent accounting, a 10 GB/1,000-execution result, production SLO evidence,
or a security proof for arbitrary sandbox runtimes. Hosted CI and the manual
release-artifact workflow were configured but not dispatched in this session.

## Extended distributed-system hardening

The [2026-10-09 hardening report](testing/2026-10-09-hardening.md) records the
subsequent fault campaign using the
[distributed-system-testing skills](https://github.com/shenli/distributed-system-testing):
60 abrupt worker deaths, real provider
faults, 1 GiB fan-out, resource exhaustion, randomized filesystem models,
186 million fuzz inputs, and an hour-long pressure run. It found and fixed
namespace-encoding and Python 3.10 heartbeat defects. An initial pressure timeout
remains an explicit unresolved finding; the later clean run does not erase it.

## Persistence inspection and reconstruction

Validated on 2026-10-09 with Go 1.26.9. This change adds advisory policy previews,
manifest v3 save reports, and explicitly approved reconstruction callbacks.

| Check | Result |
| --- | --- |
| macOS `make check` | Race-enabled Go suite, Python worker tests, vet, and formatting passed; the two Docker tests were skipped locally |
| Linux Go race suite | Passed using the Go 1.26.9 builder image on a dedicated Linux host |
| Native Linux FUSE | Existing filesystem/Git/crash-recovery tests and the new preview test passed; excluded bytes were not hydrated by preview |
| Real PostgreSQL/MinIO suite | Passed, including saved recipe/input metadata restored by a replacement worker with an empty cache |
| Python 3.10 and local Docker on Linux | All 17 SDK tests passed, including the two Docker lifecycle tests |
| Selective save example | Trusted-host and Docker reconstruction passed; retained work survived, omitted state was rebuilt, and changed recipe inputs were rejected |
| Policy examples | Python and Node selection semantics passed; no claim of package-install reproducibility for arbitrary projects |
| Documentation | Local links/anchors and Python snippet syntax checked; private planning files remain excluded |

Linux tests used the dedicated `~/work/ramet-persistence-review` checkout and
isolated PostgreSQL/MinIO services, which were stopped afterward. A compiled
policy-fixture test initially ran from the repository root instead of its Go
package directory; rerunning from `engine/` passed. No application code change
was required for that invocation issue.

The [AX assessment](ax-integration.md) is based on pinned upstream source and
documentation. No AX/Substrate cluster integration was executed. Preview byte
counts are logical estimates, and successful reconstruction verification is
application-defined; these checks do not establish automatic writeback,
reproducibility against changing external package sources, or production SLOs.

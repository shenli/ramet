# Extended alpha hardening — 2026-10-09

The extended campaign found and fixed two reproducible defects: lossy manifest
encoding for non-UTF-8 names, and broken automatic heartbeats on Python 3.10.
One earlier pressure run timed out; a subsequent one-hour run passed, but the
original timeout remains an unresolved availability finding. These results
support further alpha testing, not a production-readiness claim.

The methodology follows the [design](https://github.com/shenli/distributed-system-testing/blob/641486130536153066a3a6353cd188176cc05d28/skills/designing-distributed-system-tests/SKILL.md)
and [execution](https://github.com/shenli/distributed-system-testing/blob/641486130536153066a3a6353cd188176cc05d28/skills/executing-distributed-system-tests/SKILL.md)
skills at commit `641486130536153066a3a6353cd188176cc05d28`: identify guarantees,
inject faults, retain operation histories, verify that faults actually affected
the worker, and check acknowledged state after recovery.

## Results

| Test | Executed coverage | Result |
| --- | --- | --- |
| Full suites | Linux race suite and vet, three shuffled schedules; macOS `make check`; post-fix checks | Passed |
| Real providers | Five complete PostgreSQL/MinIO runs, 40 top-level test executions | Passed |
| Native FUSE | 20 repetitions each of filesystem, killed-worker recovery and Git tests; post-fix repeats | Passed |
| Provider faults | Three seeds × five rounds of corrupt/truncated/delayed reads, denied uploads, lost upload replies and lost metadata replies | Failed operations did not acknowledge partial state; 60 acknowledged snapshots reconstructed from an empty cache |
| Owner fencing | 30 exact-session PostgreSQL terminations | All stale owners rejected; replacement restored prior captures |
| Abrupt worker death | 60 SIGKILL cycles, copy/lazy, three seeds; same-root mount recovery then empty-cache replacement | 60 acknowledged captures survived; 330 reconstructions; 60 interrupted operations remained explicitly unknown |
| Git state | SHA-1 and SHA-256; copy/lazy; three seeds; 60 commits per case plus staged/unstaged/untracked/dangling objects and repack | Offline reconstruction matched object inventories, HEAD, index, status and selected bytes |
| Filesystem model | 60,000 mutations over 30 seeds per backend; 3,000 fork/promote/restore sequences | Matched an independent file map; reset and stale-handle checks passed |
| POSIX edges | Exchange rename, directory rename, open/unlink, hardlinks, mmap/msync, read-only/empty files and symlinks | Passed on copy and lazy |
| Larger fan-out | 1 GiB / 256 objects, 64 MiB cache; 8 copy and 100 lazy executions | Four repetitions passed (three during concurrent stress); correct sampled bytes, reset and accounting; lazy acquisition downloaded no file bytes |
| Exhaustion | Real ENOSPC in private 32 MiB tmpfs; EMFILE in a subprocess with a 96-fd limit; three repetitions each | No partial manifest; prior captures and subsequent healthy captures restored |
| Serialization fuzzing | 186,335,580 total inputs; includes 20 minutes each for strengthened filename and symlink round-trip targets | No further failures; initial weaker oracle missed the encoding bug |
| SDK | Nine tests on Python 3.10–3.14; installed wheel tested separately on Python 3.10 | Passed, including automatic renewal across a two-second command with one-second leases |
| Sustained pressure | 3,605.48 seconds, 12 workers, copy/lazy, trim/prefetch/GC during lifecycle work | 83,660 successful cycles; zero errors; zero final pins/reservations/idle slots |

The hour-long run completed 8,947 copy and 74,713 lazy cycles. RSS peaked at
62.4 MiB and ended at 42.8 MiB; descriptors ranged from 35 to 80. Both caches
stayed within 1 MiB. The two stores performed 303,452 evictions. Expected trim
admission failures under pinned pressure are separate from workload errors.
Two additional simultaneous 10-minute runs passed during repeated 1 GiB fan-out:
14,606 cycles in 604.24 seconds and 14,509 cycles in 602.31 seconds, with zero
errors. The three clean stress runs total **112,775 cycles**. This was a
closed-loop stress workload, not a latency or fairness benchmark.

## Defects and remaining finding

**F1 — namespace encoding, fixed.** Linux permits filenames with invalid UTF-8.
Go JSON serialization replaced those bytes without reporting an error. A
successful snapshot could therefore restore a different filename or symlink
target. Capture now rejects unsupported encoding before publication, including
policy recipes and Git provenance. Permanent regressions check rejection and
absence of publication; fuzzing checks actual JSON round-trip equality rather
than comparing validation to its own helper predicate.

**F2 — Python 3.10 heartbeat, fixed.** The supported minimum Python version
could not parse arbitrary nanosecond fractions in Go timestamps. Its heartbeat
thread crashed and short leases expired. Parsing now normalizes precision to
microseconds, and malformed timestamps become recorded heartbeat errors.
Integration tests fail on unexpected background-thread exceptions. Runtime
checks cover Python 3.10 through 3.14 and the built wheel.

**F3 — pressure timeout, unresolved.** The initial 12-worker run hit a prefetch
HTTP 408 and six lazy-client timeouts after at least 8,204 successful cycles,
while other fault, fuzz and scale jobs shared the host. It shut down normally;
no acknowledged-state loss was observed. The one-hour rerun with test-only
stack-dump instrumentation passed. This does not identify the original cause
or justify classifying it as harmless environment noise. Two additional simultaneous 10-minute reruns, overlapping three repeated 1 GiB
fan-outs, also passed with diagnostic stack samples. The timeout was not
reproduced; its component classification remains unknown.

## Evidence and limits

All remote work used `~/work/ramet-phase2` on a dedicated Linux test host:
Linux 6.8/ext4, Go 1.26.9, PostgreSQL 16.14, MinIO
`RELEASE.2025-09-07T16-13-09Z`, Docker 29.8.1 and Git 2.34.1. Tests used dedicated
provider containers, namespaces and object prefixes. No shared application
databases, host clock, firewall or production credentials were changed.

The local ignored session directory
`test-sessions/phase2-hardening/20261009/` retains the plan, reusable external
Go/Python harnesses, raw histories, logs, source hashes and detailed findings.
An independent history checker consumed 65,345 recorded operations, validated
required fields and unique operation IDs, and reconciled all 120 acknowledged
provider/crash captures. Negative controls reject missing or incorrect restored
values. Raw worker data and large fixtures remain under the remote test checkout.

The skill's session verdict is **FAIL** because reproducible defects were found;
post-fix checks pass, with F3 still open. A broad tenant-boundary verdict remains
**PARTIAL-surface**: API and metrics regressions ran, but hostile CLI, audit-log
and background-job boundary matrices were not completed. This campaign does not
establish power-loss behavior, multi-node consistency, a 10 GiB / 1,000-execution
target, arbitrary-byte filename support, or production latency/fairness SLOs.
The one-hour worker included a test-only signal handler for goroutine dumps;
release binaries contain no diagnostic overlay. Rebuilt release archives, wheel
and non-root Docker image were checked; the final image passed 100 demo cycles.
Dedicated test services were stopped and no test FUSE mounts remained. Source
changes are uncommitted; no release was published. Hosted CI was not run.

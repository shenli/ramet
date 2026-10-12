# Benchmarks and soak workloads

A checked-in [local result](benchmarks/macos-arm64-local.json) and its
[command/source metadata](benchmarks/macos-arm64-local.metadata.json) record
1,000 successful cycles at concurrency 16 on macOS arm64. All file
materializations used copy-on-write, with no verification failures, quarantines,
or content-store writes during the execution loop. This is a small-workload
smoke measurement on a development machine, not a production capacity claim.

Build with `make build`. Run a quick self-contained workload:

```sh
./bin/ramet demo --concurrency 8 --iterations 100
```

The generated base contains 32 different files totaling about 2.5 MiB. Every
attempt checks that the previous attempt's marker is absent, writes a marker,
and releases the execution through a complete reset and deep verification.

For a representative quiesced Git-aware environment, include the working tree,
Git metadata, and dependencies in a directory outside Ramet's storage:

```sh
mkdir -p benchmark-results
./bin/ramet bench --source /path/to/base --concurrency 32 --iterations 1000 \
  > benchmark-results/local-32.json
./bin/ramet bench --source /path/to/base --concurrency 128 --iterations 100000 \
  --require-cow > benchmark-results/soak-128.json
go test -run '^$' -bench BenchmarkAcquireRecycle -benchmem ./engine
```

The runner creates temporary worker storage on the OS temporary volume. Set
`TMPDIR` to a directory on the filesystem you want to measure. Clones are only
possible within compatible filesystems, and the temporary volume may differ
from the source volume. Initial import copies into that volume before execution
cloning. Temporary state is cleaned up on normal exit.

Results include OS, architecture, Go version, concurrency, successful attempts,
elapsed time, throughput, snapshot duration, cold/warm readiness and recycle
p50/p95/p99 latency, base logical bytes, clone/copy counts, and content bytes
written during the execution loop. The first cohort is materialized serially to
measure per-execution cold latency; warm loop operations run concurrently. Setup
and cold preparation are excluded from sustained-loop throughput.

`durable_bytes_during_executions` should be zero for the discard/recycle workload.
It measures new bytes in the **local** content store, not network activity. The
remote fan-out gate additionally checks the remote upload/download counters.

The JSON records heap allocation after the run, not peak memory or a memory-leak
proof. Latency samples consume memory proportional to iteration count. For a
long soak, repeat bounded runs and also collect OS RSS, open descriptors, disk
pressure, elapsed time, and error counts externally. For a running worker, scrape
`/metrics`; distinguish expected drift verification on active dirty workspaces
from reset/acquire failures. Any quarantine or verification failure in the clean
benchmark path is a failed run, never silently excluded from throughput.

## Reporting results

Report the source dataset and commit, file count and logical size, hardware,
filesystem, OS/kernel, Ramet commit, Go version, cache state, concurrency,
iterations, and whether fallback copies occurred. Run multiple trials; compare
the same workload and verification guarantees.

Do not report `du` or summed `st_blocks` as physical COW amplification: shared
extents can be counted multiple times. This version reports logical bytes and
clone counters only. Filesystem-specific exclusive/shared-extent accounting is
required to substantiate physical amplification. The repository does not claim
a measured 10 GB / 1,000-execution result.

Copying a base directory, `git worktree`, and runtime-native snapshots are useful
baselines, but record their reset, verification, and process-isolation contracts.
Ramet's current reset reconstructs and deeply verifies the tree; file count and
bytes scanned can dominate latency even when data extents are shared.

## Phase 1 Linux fan-out

[Measured result](benchmarks/phase1-linux-fanout.json): three rounds of 1,000
simultaneously acquired slots, 3,000 successful attempts, zero verification
failures/quarantines, and zero local or remote content writes during the loop.
The fixture is a pinned Git repository, 262,529 logical bytes including Git state.
Thirty-two client threads mutate and recycle the cohort. Batch readiness was
0.751 seconds cold and 0.122–0.135 seconds warm; attempt/recycle throughput was
552–1,016 per second. These are small-base smoke measurements, not 1,000 CPU-bound
agents executing simultaneously. Linux used independent copies, not reflinks.

Reproduce on an isolated Linux Docker host with `./scripts/run-phase1-gate.sh`.
It starts dedicated PostgreSQL/MinIO test services, runs race and provider tests,
then runs the fan-out workload and validates doctor/drain. Test data and caches
stay under `.test-services` and `.test-cache` in the checkout. Do not point the
integration suite at a production database: schema-rejection tests deliberately
change its version marker. Stop test services with
`docker compose -p ramet-phase1-test -f compose.integration.yaml down`.

The earlier macOS result is the Phase 0 baseline (`6243d78`), not a measurement
of the new remote provider path. Large bases, prolonged pressure, physical extent
accounting, and production workload throughput remain Phase 2 validation.


## Phase 2 sustained pressure workload

Run `scripts/run-phase2-gate.sh` on Linux with Docker and FUSE. The default
30-minute run uses two credential-scoped stores (copy and lazy), eight workload
threads, a 4 MiB/64-file base and a 1 MiB remote cache per tenant. It repeatedly
reads and modifies private files, resets/recycles, forks and promotes/restores
selected attempts while prefetch, eviction and GC run concurrently.

The JSON report records actual elapsed time, per-backend cycles and cycle
latencies, cold/warm acquisition measurements, periodic RSS/descriptor/thread/
disk samples, cache counters and source/binary hashes. Cycle latency includes
occasional fork and durable promotion, so it is not a pure acquisition latency
metric. Both tenants share a host and provider services; results are not isolated
backend microbenchmarks. The lazy backend defers unread bytes, while copy eagerly
materializes the base; read amplification and workload access patterns matter.

An external Docker attempt verifies a single-workspace mount with bounded CPU,
memory/PIDs, disabled networking, no capabilities and a read-only root. This
validates the reference runtime contract, not a general sandbox security proof.
Use longer durations and representative repositories to qualify your workload;
retain resource trends rather than only reporting final memory or throughput.
## Workspace comparison

Run the same deterministic file mutations and independent byte/namespace checks
against full directory copies, Git worktrees, and a same-host Ramet worker:

```sh
make build
python3 -m pip install ./sdk/python
./bin/ramet dev --data /tmp/ramet-comparison-worker
# Another terminal; use a new output directory for each run:
python3 scripts/compare-workspaces.py --output .ramet/comparison \
  --files 128 --file-bytes 65536 --attempts 16 --concurrency 4
```

The script saves `report.json` and its generated source. It deletes its own
snapshot after closing pools; local orphan objects remain eligible for normal GC.
Use a dedicated worker. Set `RAMET_URL`/`RAMET_TOKEN` when necessary.

A Linux ext4 smoke measurement (8 MiB payload, 128 files, 16 cycles per strategy,
concurrency 4, copy fallback) produced:

| Strategy | Median prepare | Median reset | Verified cycles/s |
| --- | ---: | ---: | ---: |
| Full copy | 49.80 ms | 59.11 ms | 26.31 |
| Git worktree | 13.38 ms | 14.29 ms | 82.21 |
| Ramet | 24.31 ms | 49.51 ms | 29.08 |

[Raw report](benchmarks/prelaunch-linux-comparison.json). All 48 cycles passed
the common verification. Initial Ramet capture took 3.65 seconds and is reported
separately. This small sample is a functional comparison, not a performance SLO.

Git worktrees were fastest in this case, but share repository administration;
they do not supply Ramet's independent Git metadata or verified lifecycle.
The two full-tree strategies include independent `.git` directories. Ramet's
internal verification is included in its API times; the common verifier is
outside every strategy's prepare/reset timer and inside total wall time.
Reset recreates the tree in all three strategies; it does not benchmark
`git reset --hard`/`git clean` reuse, which can be faster and has a different
cleanup contract. Python copytree may use filesystem copy acceleration.

Strategy order is fixed and host page caches are not flushed. Do not describe
these as cold-start measurements. Physical shared extents, remote I/O savings,
large-base scaling and agent inference cost are not measured here. Reproduce on
your own repository and filesystem before extrapolating.

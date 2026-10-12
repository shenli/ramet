# Developer guide

This guide is for contributors changing Ramet itself. For a harness integration,
start with the [Python SDK](../sdk/python/README.md), [Go integration](go.md),
and [operations contract](operations.md). Read [architecture](architecture.md)
and [implementation design](design.md) before changing storage or lifecycle code.

## Prerequisites and first run

Use Linux or macOS, Go 1.26.9 or newer, Python 3.10 or newer, and Git. A C toolchain
is needed for Go's race detector. Docker with Compose is optional for local work
and required for the documented PostgreSQL/MinIO integration environment. The
local mode requires no cloud account, API key, database, or model provider.

From the repository root:

```sh
make build
./bin/ramet version
./bin/ramet demo --concurrency 8 --iterations 100
make check
```

The demo creates its own fixture and temporary store. `make check` runs all Go
tests with the race detector, builds the CLI, runs Python integration against a
real worker, runs `go vet`, and checks Go formatting. Real provider tests skip
unless their service environment variables are set; a successful local check
alone does not prove PostgreSQL/S3 behavior.

The installed toolchain must meet `go.mod`. Go may download a compatible
toolchain and modules on the first run. Format Go changes with `make fmt`.
The Python SDK uses only the standard library at runtime.

## Run a worker while developing

```sh
make build
export RAMET_TOKEN=local-development-only
./bin/ramet dev --data /tmp/ramet-dev --listen 127.0.0.1:7433
```

In another terminal, use the same token:

```sh
export RAMET_TOKEN=local-development-only
./bin/ramet health
./bin/ramet doctor
python3 -m venv .venv
.venv/bin/python -m pip install -e ./sdk/python
```

Keep snapshot sources outside `/tmp/ramet-dev`; source/store overlap is rejected.
The data directory must be private to the worker's Unix user. `doctor` reports
whether the actual volume supports cloning. Copy fallback is a supported mode;
use `--require-cow` only when testing a workload that requires clone support.

A running worker exposes `/live`, `/ready`, `/metrics`, and the `/v1` API. Use
`RAMET_URL` to point CLI/SDK clients at a different address. SIGTERM drains the
worker and discards ephemeral state; snapshots remain in the store.

## Repository layout

```text
engine/             Public Go state engine, models, and lifecycle tests
storage/            Durable provider interfaces and PostgreSQL/S3 implementations
internal/server/    Private HTTP API and transport tests
client/             Go HTTP client
cmd/ramet/          CLI and worker entry point
internal/           Configuration, filesystem helpers, and benchmark runner
integration/        Tests against real storage services
sdk/python/         Python SDK and real-worker integration tests
configs/            Example worker configuration
scripts/            Integration and fan-out gates
docs/               Architecture, design, operations, and developer guides
examples/           Harness integration examples
third_party/        Dependency licenses and notices
```

The repository root contains project metadata, build/deployment configuration,
and top-level documentation. Embedded Go applications import
`github.com/shenli/ramet/engine`; HTTP users import
`github.com/shenli/ramet/client`. The module root is not a Go package.
The public Go packages are `engine`, `client`, and `storage` (including its
provider implementations). HTTP server wiring is private in `internal/server`;
external applications use the engine or HTTP client. Keep private support code
in `internal/` and state semantics in `engine/`.

## Find the code to change

| Change | Start here | Relevant tests |
| --- | --- | --- |
| Capture, file fidelity, promotion policy | `engine/content.go`, `engine/types.go` | `engine/ramet_test.go`, `internal/fsutil/fs_test.go` |
| Prefetch, locality, explicit pool warming | `engine/preparation.go` | `engine/preparation_test.go`, provider and SDK tests |
| Acquire, reset, fork, quotas, expiry | `engine/pool.go`, `engine/leases.go` | `engine/ramet_test.go`, `engine/leases_test.go` |
| Restart, drain, deletion, GC, doctor | `engine/store.go` | `engine/ramet_test.go`, `engine/leases_test.go` |
| Git checkout, pack partition and reconstruction | `engine/git.go`, `engine/git_objects.go` | `engine/git_objects_test.go` |
| Bounded remote cache and idle expiry | `engine/cache.go`, `engine/idle.go` | `engine/pressure_test.go`, `integration/phase2_test.go` |
| Linux lazy hydration | `internal/lazyfs/`, `engine/execution_fs.go` | Linux tests in those packages, `engine/lazy_test.go` |
| Tenant routing and metrics | `internal/server/tenants.go`, `internal/server/observability.go`, `cmd/ramet/tenants.go` | `internal/server/tenants_test.go`, Linux soak |
| Remote publication and hydration | `engine/remote.go`, `storage/` | `integration/integration_test.go` |
| YAML, CLI options, commands | `internal/config/`, `cmd/ramet/` | Tests in those directories |
| HTTP route or error mapping | `internal/server/`, `client/` | `internal/server/server_test.go` |
| Python client and environment adapter | `sdk/python/ramet/client.py` | `sdk/python/tests/test_integration.py` |

Keep core semantics in the engine, so embedded users and HTTP clients receive
the same behavior. Treat the [operations API table](operations.md#api-and-cli)
as the public route reference rather than maintaining a second route list here.

## Focused test loop

```sh
# Core lifecycle and leases, including the race detector.
go test -race -count=1 ./engine

# HTTP, command-line, and configuration behavior.
go test -race -count=1 ./internal/server ./cmd/ramet ./internal/config

# Real worker + Python SDK; builds the current binary first.
make test-python

# Manifest path and symlink validation fuzzing.
go test -run '^$' -fuzz FuzzManifestPaths -fuzztime 30s ./engine
```

Tests should exercise observable guarantees: no contamination after reuse,
stale handles rejected, quota returned after failure, no publication after an
upload failure, and recovery from durable state. Avoid tests that only mirror
the implementation. Use contexts with bounded deadlines for wait/cancellation
tests. Race checks matter for changes to lifecycle and locking.

Before sending a change, run the focused tests it affects and `make check`.
Provider changes also require the integration suite below. Use the benchmark
only when the change affects performance or there is a specific regression to
investigate; report the workload, filesystem, and verification guarantees.

## PostgreSQL and MinIO integration

Use the dedicated test stack from the repository root:

```sh
docker compose -p ramet-phase1-test -f compose.integration.yaml up -d --wait
make test-integration
docker compose -p ramet-phase1-test -f compose.integration.yaml down
```

The services bind loopback ports 17432 (PostgreSQL) and 17900 (MinIO). Their data
persists under `.test-services/`, which is ignored by Git and Docker builds.
The script supplies test-only credentials and a dedicated database/bucket; the
tests create the bucket when needed. `down` stops containers without deleting
the bind-mounted test data.

**Use an isolated database.** These tests terminate an ownership connection and
temporarily change the schema version marker. They are unsuitable for a shared
or production Ramet database, even if a unique namespace is selected. Avoid
concurrent copies of the integration suite against the same database.

To use other dedicated test services, set these variables before invoking
`make test-integration`:

| Variable | Purpose |
| --- | --- |
| `RAMET_TEST_POSTGRES` | PostgreSQL DSN; test role must be able to perform schema and connection-loss tests |
| `RAMET_TEST_S3_ENDPOINT` | HTTP(S) S3-compatible origin |
| `RAMET_TEST_S3_BUCKET` | Dedicated test bucket |
| `RAMET_TEST_S3_ACCESS_KEY` | Test access key |
| `RAMET_TEST_S3_SECRET_KEY` | Test secret key |

The integration suite tests immutable-provider conformance, empty-cache worker
replacement, publication failures, PostgreSQL ownership loss, and rejection of
newer schemas. Service reachability alone is not a substitute for these tests.

## Linux fan-out gate

On a dedicated Linux Docker host, from a checkout on the filesystem to test:

```sh
./scripts/run-phase1-gate.sh
```

This runs the race suite and real provider suite using a containerized Go
toolchain, builds a host worker, checks migrations/readiness, runs three cohorts
of 1,000 execution slots, verifies doctor, and checks drain. It uses host
networking for the build/test container and is intended for Linux. The worker
listens on loopback port 17433. Cached modules, binaries, worker logs, fixtures,
and worker data stay under `.test-cache/`.

For a smaller local investigation:

```sh
RAMET_FANOUT_CONCURRENCY=100 RAMET_FANOUT_ROUNDS=3 ./scripts/run-phase1-gate.sh
```

The script overwrites `docs/benchmarks/phase1-linux-fanout.json` with the new
result. Review it before committing; a changed JSON file is not a replacement
for matching source/workload provenance. Services remain available after the
gate; stop them with the Compose `down` command above. Existing remote test
work follows the same layout under `~/work/ramet-phase1`, rather than using a
host's unrelated databases or buckets.

See [benchmarking](benchmarking.md) for measurement boundaries and reporting.

## Extend the storage layer

Implement the interfaces in [storage/storage.go](../storage/storage.go).
`Metadata` must hold exclusive namespace ownership until close, fail closed
when ownership is lost, publish immutable records atomically and idempotently,
and reject deletion of referenced parents. `Content.Put` must validate input,
confirm durable success, and preserve immutable digest keys. Its boolean result
means newly added content for accounting. `Open` must return missing-object
errors reliably, even when the underlying SDK delays the request.

Wire the pair through `engine.Options` for embedded use. `OpenContext` takes
ownership of `Metadata` and closes it even on failed initialization; avoid
reusing that handle afterwards. For worker configuration, extend the explicit
provider construction in `internal/config` with strict validation. There is no
dynamic plugin loader or remote object-deletion interface.

Add real provider conformance and failure tests before claiming support. Include
content corruption, missing objects, duplicate publication, ownership loss,
parent deletion guards, partial upload, and empty-cache restoration. Preserve
upload-before-metadata ordering in the engine.

## Change an API or format

For an API change, update the engine model, HTTP validation/error mapping, Go
client, CLI where applicable, Python binding, and the relevant public docs.
Preserve the single-result behavior of `acquire()` when adding batch features.
Include a real-worker SDK test for new user-visible lifecycle behavior.

For durable formats, document version compatibility and migration behavior
before changing writers. Keep fixtures for older readable formats and rejection
tests for newer unsupported versions. PostgreSQL migration tests must cover
transactional failure and ownership behavior. Do not silently reinterpret
existing manifest bytes or reuse a version for an incompatible schema.

## Package and inspect

```sh
python3 -m venv .venv
.venv/bin/python -m pip install build
.venv/bin/python -m build sdk/python
.venv/bin/python -m pip install --force-reinstall sdk/python/dist/*.whl
.venv/bin/python -m unittest discover -s sdk/python/tests -v

docker build -t ramet:dev .
docker run --rm ramet:dev demo --concurrency 4 --iterations 20
```

Run installed-wheel tests without `PYTHONPATH` pointing at the source SDK; this
catches missing package files. The worker binary must already exist at
`bin/ramet`, or set `RAMET_TEST_BINARY` to an absolute path. Wheel contents must
include `py.typed`, LICENSE, and NOTICE. Preserve third-party notices in the
container when dependencies change.

[CI](../.github/workflows/ci.yml) runs Linux/macOS Go checks, fuzzing, packaging,
container smoke tests, and a separate durable-provider job. A workflow definition
is not evidence of a completed run; record the actual checks performed in a PR.

## Maintain the documentation

[Architecture](architecture.md) describes the implemented component boundaries;
[implementation design](design.md) records current invariants and tradeoffs.
Keep future proposals in the product design and status roadmap. Update
[operations](operations.md) when flags, configuration, endpoints, or recovery
procedures change. Add new guides to the [documentation index](README.md).

The detailed architecture image has an editable [draw.io source](images/architecture.drawio).
The README uses a separate [overview source](images/overview.drawio) to keep its
introduction focused on the harness, workspace, runtime, and storage boundaries.
After editing a diagram, regenerate its PNG from the repository root with draw.io desktop:

```sh
drawio -x -f png -s 1.5 -b 20 -o docs/images/architecture.png docs/images/architecture.drawio
drawio -x -f png -s 2 -b 20 -o docs/images/overview.drawio.png docs/images/overview.drawio
```

Some installations name the executable `draw.io`. Inspect the exported image
for clipped labels and crossing connectors, and keep the image and source in
the same change. Check relative links and `git diff --check` for documentation
changes; application tests are needed only when executable behavior also changes.

## Debugging and review

| Symptom | What to inspect |
| --- | --- |
| `409` on an execution operation | Current pool/ID, lease, generation, and expiration; reset replaces the handle |
| `429` | Active/reserved, idle, request admission or cache pressure; inspect cache status and metrics |
| Acquire waits | Pool capacity, held leases, caller deadline, and drain state |
| `422` or quarantine | Verification result, source mutation, cached object integrity, and filesystem support |
| Readiness fails in remote mode | Provider reachability and original PostgreSQL ownership session |
| Provider tests are skipped | Missing `RAMET_TEST_POSTGRES`; use `make test-integration` with test services |
| Low throughput or high disk use | Clone/copy and hydration counters, base/file sizes, cache misses, idle limit/TTL |
| Lazy file open returns I/O error | Worker hydration error log, remote availability, cache object integrity, headroom |
| Lazy startup/cleanup fails | `/dev/fuse`, fusermount availability, runtime mounts/open handles, owned mount recovery |

Capture version, configuration with secrets removed, filesystem details, and a
minimal reproduction. Do not include source contents, credentials, or execution
lease tokens in public reports. See [contributing](../CONTRIBUTING.md) for review
expectations and [security](../SECURITY.md) for private vulnerability reports.


## Phase 2 Linux gate

Use a dedicated Linux checkout, for example `~/work/ramet-phase2`. Install
`fusermount3` and ensure your user can open `/dev/fuse`. The gate builds with the
pinned Go Docker image, then runs FUSE tests natively on the host; mounting FUSE
inside the test container is not required.

```sh
./scripts/run-phase2-gate.sh
# Full default: race suite, native FUSE, real providers, SDK, 30-minute soak.
# Use a short duration only when debugging the gate itself:
RAMET_SOAK_SECONDS=60 ./scripts/run-phase2-gate.sh
```

The soak starts a credential-routed copy tenant and lazy tenant with independent
PostgreSQL namespaces, S3 prefixes and 1 MiB caches for a 4 MiB base. It runs
acquire/read/write/reset/release, forks and durable promotion/restoration while
trimming, prefetching and collecting. Samples record process RSS, descriptors,
threads, physical local disk use, cache limits and cycle counts. Reports retain
source hashes and the tested binary hash. A separate Docker attempt checks CPU,
memory, process, network, capability and mount restrictions.

Test credentials target only the disposable integration stack. The script leaves
those containers running for inspection; stop them explicitly after testing:

```sh
docker compose -p ramet-phase2-test -f compose.integration.yaml down
```

Do not sync internal marketing/planning files to a test host or include them in
artifacts. Do not rewrite historical benchmark JSON when updating code: add new
evidence with the actual source, toolchain, duration and filesystem.

## Release artifacts

```sh
bash scripts/build-release.sh
uv build sdk/python --out-dir .test-cache/python-dist
```

The binary script builds Linux/macOS amd64/arm64 archives, includes Apache-2.0
and linked-dependency notices, and writes SHA-256 checksums. The manual
[artifact workflow](../.github/workflows/release.yml) builds the same candidates;
it does not tag the repository, publish a GitHub release or upload to PyPI.
See [release checklist](release.md) before publishing.

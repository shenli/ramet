# Contributing to Ramet

Ramet is an early implementation of state infrastructure for parallel agent
environments. Correct reset and isolation take priority over benchmark peaks.

Start with the [developer guide](docs/development.md) for setup, focused tests,
real PostgreSQL/MinIO integration, packaging, and extension workflows. Read the
[architecture](docs/architecture.md) and [implementation design](docs/design.md)
before changing lifecycle or persistence semantics.

## Develop

Install Go 1.26.9 or newer, Python 3.10 or newer, and Git. Supported worker platforms
are Linux and macOS. Run:

```sh
make check
make demo
```

`make check` runs the Go race detector, HTTP/CLI tests, Python tests against a real
worker, formatting checks, and `go vet`. No credentials or external services are
needed. `go test -run '^$' -fuzz FuzzManifestPaths -fuzztime 30s ./engine` exercises
manifest path validation. CI runs on Linux and macOS.

For a bug report, include the version, OS/filesystem, `ramet doctor` result,
reproduction steps, and expected behavior. Remove credentials, source contents,
and execution leases from logs before posting.

## Change expectations

- Keep public APIs about snapshots, pools, and execution lifecycle.
- Never share writable hardlinks with immutable content or another execution.
- Test errors and interrupted lifecycle operations as well as successful paths.
- Never acknowledge a durable manifest before all referenced content is synced.
- Every state format change needs compatibility tests and a migration plan.
- Document performance measurements with their workload and filesystem.
- Keep implementation status current; do not describe roadmap features as shipped.

For substantial architecture changes, open an issue to discuss the problem and
tradeoffs before building a large patch. Small fixes can go straight to a PR.
Use `gofmt` and keep Python free of runtime dependencies where practical.

By submitting a contribution, you agree to license it under the repository's
Apache-2.0 license. Keep copyright notices and add attribution when incorporating
third-party work.

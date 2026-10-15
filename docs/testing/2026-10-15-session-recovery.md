# Pinned-environment session recovery validation

Local validation on 2026-10-15. These results cover the Python SDK's dependency
compatibility helper and the executable document session workflow. They are
correctness checks, not latency or scale measurements.

## Executed checks

| Check | Result |
| --- | --- |
| `make check` | Passed: Go race suite, CLI build, Python tests, vet, formatting |
| Python SDK with real Docker enabled | 29 tests passed, no skips |
| Python SDK on real Linux FUSE | 26 passed; 3 Docker tests skipped in this container and executed separately above |
| DOCX session with local worker storage | Passed: worker restart, fresh containers, dependency reuse, lockfile-triggered rebuild, second save/restore |
| DOCX session with PostgreSQL and MinIO | Passed with an empty replacement worker directory; document restored byte-for-byte and remained editable |
| Public-name scan and new guide links | No excluded names found; links resolved |

The document workflow used Python 3.12 with hash-pinned python-docx 1.2.0,
lxml 6.1.3, and typing-extensions 4.16.0. The host was macOS arm64 with a local
Docker Desktop Linux daemon. Worker replacement used two sequential processes
on that host; it was not a multi-host scheduling test. PostgreSQL 16 and MinIO
ran in the disposable integration Compose project. The FUSE SDK suite ran in
Linux with `/dev/fuse`, using the copy of Ramet built in that container.

The DOCX example made exactly one installation for the initial session and
none on compatible restore. Changing the lockfile caused one further
installation; subsequent restore reused that result. The rebuild removed a
stale dependency file while retaining the original document's SHA-256.

Regression tests also cover image/platform/mount-path/input mismatch, invalid
markers, missing dependency files, failed callbacks, failed readiness checks,
input changes during verification, symlink traversal, cleanup/lease failure,
and dependency retention versus policy exclusion through the real worker API.

## Reproduce

Follow the [document session example](../../examples/document_session/README.md).
For the disposable providers used here, start `compose.integration.yaml`, create
the `ramet-phase1-integration` bucket, set `RAMET_DATABASE_URL`,
`RAMET_S3_ACCESS_KEY`, and `RAMET_S3_SECRET_KEY`, and pass
`examples/document_session/providers.test.yaml` to `--worker-config`.
The existing Go integration suite also provisions that bucket.

The CI workflow now includes both document recovery modes. Hosted CI was not
run for these uncommitted changes. Package restoration does not certify arbitrary
virtualenvs, rootfs changes, document rendering fidelity, or application behavior
outside the exercised DOCX operations.

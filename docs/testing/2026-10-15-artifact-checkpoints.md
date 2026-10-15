# Large-artifact checkpoint validation

This change adds content-defined chunks for large files and removes repeated
S3 body reads when a native full-object SHA-256 confirms existing content.

## Document comparison

[Raw results](../benchmarks/artifact-checkpoints-linux.json) compare whole-file
storage with chunk storage using the same updated S3 provider. The fixture
generator and dependencies are checked in, including the uv script lockfile.
See [reproduction instructions](../artifact-checkpoints.md#reproduce-the-document-comparison).

Environment: Linux arm64 in Docker Desktop, Go 1.26.9, a test container limited
to four CPUs and 6 GiB RAM, and PostgreSQL/MinIO containers on the same local
Docker network. Workspaces and cache use disposable container-local storage.
Fixture files are read from a host bind mount. The run uses no race detector,
one sequential trial per strategy, and no artificial network latency.

Each revision contains a DOCX, XLSX, PPTX, and PDF totaling approximately
51.15 MiB of logical file content. Each document embeds the same deterministic
12 MiB noise image. This tests **media-heavy documents with shared inputs**;
cross-file chunk reuse contributes to the savings. It is not a representative
sample of all knowledge-work artifacts. Both strategies receive identical bytes.

| Revision | Whole-file upload | Chunk upload | Whole-file save | Chunk save |
| --- | ---: | ---: | ---: | ---: |
| Initial capture | 51.22 MiB | 41.13 MiB | 342 ms | 390 ms |
| Unchanged checkpoint | 0 | 0 | 152 ms | 216 ms |
| Text edits, original media | 51.22 MiB | 23.52 MiB | 319 ms | 319 ms |
| Regenerated media | 51.23 MiB | 29.99 MiB | 329 ms | 379 ms |

The text-edit checkpoint uploaded approximately **54% fewer body bytes**.
The regenerated-media result also benefits from sharing new image content
between the different files; it is not purely reuse of the previous revision.
HTTP body counts can include SDK signing framing. The raw report separately
records new unique content bytes and request counts.

Both strategies performed zero S3 GETs during these saves. Chunking used more
PUT requests: seven rather than four for the text edit, and 25 rather than four
for regenerated media. Cold restore took 118–137 ms with whole-file objects
and 150–170 ms with chunks. Each restore used a new worker cache and passed an
independent file-name and SHA-256 comparison against the original documents.

These results demonstrate byte reuse and exact recovery. They do **not** establish
an overall latency improvement, a storage billing reduction, a production SLO,
or performance over a WAN. The initial fixture generator reopens files using
their document readers; this is not a visual-layout or document-quality test.

## Correctness checks

- Local Go tests cover insertion-driven offset shifts, truncation to small and
  empty files, restart, reset, GC retention, invalid chunk manifests, missing or
  corrupt chunks, reordered chunks, and full-file digest mismatch.
- Real PostgreSQL/MinIO tests interrupt an upload after a successful chunk and
  verify that no partial snapshot is published. A subsequent capture restores
  the exact original file.
- A localized edit in a 24 MiB binary file uploads one approximately 4 MiB
  chunk; an unchanged checkpoint issues no object GETs or PUTs.
- Fresh-worker recovery passes through both copy and real Linux FUSE with a
  4 MiB remote-cache budget. Cache pins and disk reservations return to zero.
  An interrupted second-chunk download fails the restore/read; retry recovers
  the complete file without exposing partial bytes as a successful read.
- Provider tests check native-checksum read avoidance, legacy/composite fallback,
  forged user metadata, incorrect native checksums, and invalid upload sources.
- The Go race suites pass on macOS and Linux, including the real provider/FUSE
  suite. `make check` covers the Python SDK, vet, and formatting as well.

The document comparison now runs in the durable-provider CI job with the race
detector for correctness. This report records local executed tests; hosted CI
has not been run for the uncommitted change.

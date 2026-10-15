# Checkpointing large agent artifacts

Ramet can preserve the working files of document-generation tasks: uploaded
inputs, extracted data, editable drafts, spreadsheets, presentations, and final
PDFs. Git is optional. Your tools continue reading and writing ordinary files
inside an attached workspace.

To restore the tools as well as the files, pair a pinned runtime image with
[persistent dependencies and a readiness check](session-recovery.md). The
[DOCX recovery example](../examples/document_session/README.md) resumes editing
in replacement containers without reinstalling compatible dependencies.

## Save at a completed tool step

Finish the tool call, close its files, and stop other writers before saving.
`execution.checkpoint()` captures all supported files, including intermediate
work. `execution.promote()` applies persistence policies and can omit explicitly
ephemeral or rebuildable paths. Do not classify intermediate analysis or drafts
as ephemeral if losing them would require repeating expensive work.

After the save succeeds, record the returned snapshot ID with your framework's
task state. To resume, create a pool from that snapshot and acquire a workspace
for the replacement sandbox. The framework owns conversation history and the
association between its checkpoint and the filesystem snapshot; those two
records are not one atomic transaction.

Use PostgreSQL metadata and S3 content when published snapshots must survive
worker-disk loss. A remote worker connection does not attach a directory to a
hosted sandbox: see [runtime integration](integration.md).

Checkpointing while a tool is actively writing is unsupported. Chunking does
not change that consistency requirement or make unsaved work durable. A worker
failure can still lose changes since the last published snapshot.

## What is stored

- Files smaller than **8 MiB** are stored as one content-addressed object.
- Files of **8 MiB or larger** use Rabin content-defined chunking, with a 1 MiB
  boundary target, a 512 KiB minimum, and a 4 MiB maximum. The final chunk can
  be smaller than the minimum. Actual average sizes depend on the content.
- Each file records its complete ordered chunk list and a whole-file SHA-256.
  Chunks are immutable SHA-256 objects and can be reused across snapshots and
  files in the same content namespace.
- A snapshot describes the complete selected tree. Restore does not replay
  patches from older snapshots or fetch the original source files.
- A failed upload never publishes a partial snapshot. Uploaded orphan objects
  can remain; remote object deletion is not currently implemented.

Existing manifests remain readable. New captures use manifest version 4; older
workers cannot read those manifests. Upgrade readers before sharing new saves.
Git's optimized base/local packs retain their existing whole-object format.

No new SDK call is required. Chunking is enabled by default. For workloads where
whole-file storage is preferable, configure:

```yaml
worker:
  disable_chunking: true
```

Embedded Go callers can set `engine.Options{DisableChunking: true}`. This changes
new captures only; either configuration can restore existing chunked files.

## Integrity and S3 traffic

Uploads are verified locally before being sent. New single-PUT objects up to
16 MiB include S3's native SHA-256 checksum, which the server validates. A later
capture can confirm an unchanged object's size and full-object checksum with a
HEAD request, without downloading its body. User metadata, ETags, and multipart
composite checksums are not accepted as proof of the full SHA-256.

Old objects and providers without a native full-object checksum use a complete
read-and-hash fallback. A capture remembers successful durability confirmations
only for that operation, avoiding a second verification pass at publication.
It does not persist assumptions about remote objects across operations.

Restore verifies every chunk and the assembled file. Cache pins prevent a chunk
from being removed during a read. Local garbage collection, prefetch, locality,
and doctor account for chunk references. Remote cache size can be smaller than
the whole file, but must accommodate the largest chunk needed by the workload
(and concurrent pinned chunks).

## Costs and limits

Chunking reduces new stored bytes and upload traffic when file regions remain
unchanged. It still reads the retained file in full during capture; it does not
yet track dirty byte ranges. Repeated captures also stage and hash chunks locally.
An unchanged checkpoint can therefore do local I/O even with zero S3 body traffic.

DOCX, XLSX, and PPTX are compressed packages. A small visible edit may rewrite
many binary bytes. Savings depend on the generator, compression, embedded media,
and edit pattern. PDF generation can also rewrite large regions. No format is
unpacked or normalized: restore reproduces the original bytes exactly.

More chunks mean more requests and metadata. The copy backend assembles a
temporary whole file before copying or reflinking it into an execution. This
adds local I/O and staging space, and independently assembled workspaces do not
share the whole-file cache inode used by the small-file path. Linux FUSE
assembles a file directly into its private backing inode on first open; it does
not fetch individual ranges on demand. Both paths can restore more slowly than
whole-file objects.

Measure upload bytes, request counts, checkpoint latency, and restore latency
for your own files. Lower transferred bytes alone do not establish a lower total
S3 bill or a faster end-to-end workflow.

## Reproduce the document comparison

The fixture generator uses real document libraries and reopens the generated
files with their readers. It produces four revisions: a base, an unchanged
copy, a text edit retaining embedded media, and regenerated media. Each document
contains one deterministic noise image; this deliberately models media-heavy
artifacts. It is not representative of all office documents.

Start the isolated [PostgreSQL and MinIO test services](development.md#postgresql-and-minio-integration),
then run from the repository root with Go, Python 3.11+, and uv:

```sh
uv run --locked --script scripts/generate-artifact-fixtures.py \
  --output .test-cache/artifact-fixtures

export RAMET_TEST_POSTGRES='postgres://ramet_test:ramet-local-integration-only@127.0.0.1:17432/ramet_phase1_test?sslmode=disable'
export RAMET_TEST_S3_ENDPOINT=http://127.0.0.1:17900
export RAMET_TEST_S3_BUCKET=ramet-phase1-integration
export RAMET_TEST_S3_ACCESS_KEY=ramet_test
export RAMET_TEST_S3_SECRET_KEY=ramet-local-integration-only
export RAMET_ARTIFACT_FIXTURES="$PWD/.test-cache/artifact-fixtures"
export RAMET_ARTIFACT_REPORT="$PWD/.test-cache/artifact-report.json"
go test -count=1 -v -timeout=10m -run TestArtifactCheckpointWorkload ./integration
```

The output directory for fixtures must be new. The comparison saves real
snapshots with both whole-file and chunk storage. Each revision is restored
through a fresh worker cache and compared byte-for-byte via SHA-256. The report
includes library versions, file sizes, new unique content bytes, HTTP request
counts and body bytes, checkpoint time, and cold restore time. Upload body bytes
can include SDK signing framing; they are not an S3 billing calculation.

Run without the race detector for timings. CI is configured to run the same workload with the
race detector as a correctness check. See the [recorded comparison](testing/2026-10-15-artifact-checkpoints.md)
for measured results and limitations.

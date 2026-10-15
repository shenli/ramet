# Resume a document task in a new sandbox

This example installs `python-docx` into a workspace-local `.deps` directory,
creates a DOCX report, saves it, stops the worker, and resumes in fresh containers
after starting a replacement worker. It checks that:

- The original document restores byte-for-byte, and a new container can edit it.
- Matching image/platform/path/input identity reuses dependencies without reinstalling.
- A lockfile change triggers a clean dependency rebuild without losing the document.
- Rebuilt dependencies and continued work survive another save and restore.
- Ephemeral scratch files are omitted on promotion.

Requires Go, Python 3.10+, and a local Docker daemon. The image uses Python 3.12;
the package lock includes Linux amd64 and arm64 wheels. Build-time package
downloads require network access; sandbox commands run offline.

From the repository root:

```sh
make build
docker build -t ramet-document-session:local examples/document_session
SESSION_IMAGE=$(docker image inspect ramet-document-session:local --format '{{.Id}}')
PYTHONPATH=sdk/python python3 examples/document_session/run.py --image "$SESSION_IMAGE"
```

The runner refuses image tags; it inspects and launches the immutable image ID.
For deployment, publish your image and use its registry digest on each worker.
Builds performed at different times may differ because the Dockerfile's base
tag can move; the resulting image is pinned for the entire recovery test.

Default mode retains the worker's local storage across process replacement. It
proves worker restart and fresh-container recovery, not recovery from disk loss.
The temporary store and documents are removed when the example exits.

To test an **empty replacement worker directory**, pass `--worker-config` with
your disposable PostgreSQL/S3 configuration and credentials in the environment:

```sh
PYTHONPATH=sdk/python python3 examples/document_session/run.py \
  --image "$SESSION_IMAGE" --worker-config /path/to/test-worker.yaml
```

Use the copy backend and an existing S3 bucket. Both workers must use the same
metadata namespace and content prefix; don't set worker data or server settings
that override the runner's temporary-directory and loopback configuration.
See [provider configuration](../../configs/postgres-s3.yaml). Provider mode
retains published snapshots in those test services; use disposable data. It
simulates worker-disk loss on one host, not a multi-machine scheduling test.

Read [the recovery integration guide](../../docs/session-recovery.md) for the
API, compatibility rules, save-record contract, and runtime responsibilities.
The example's task is DOCX editing, not a document rendering or layout benchmark.
See the [validation record](../../docs/testing/2026-10-15-session-recovery.md)
for executed checks and their scope.

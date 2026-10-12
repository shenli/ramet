# Linux FUSE workspaces

Ramet's `lazy` backend exposes each execution as a writable FUSE filesystem.
Acquire prepares the directory tree and file metadata; opening a file fetches
and verifies its bytes into a private local inode. This is useful when an agent
touches only part of a large snapshot. Writes stay local until an explicit
checkpoint or promotion, just as with the default copy/reflink backend.

FUSE is optional and Linux-only. It does not provide a network filesystem or
make an execution path accessible to a remote sandbox.

## Start a worker

Build Ramet following the [quick start](../README.md#quick-start). On a native
Debian/Ubuntu Linux host, install the FUSE helper and enable the kernel device:

```sh
sudo apt-get install fuse3
sudo modprobe fuse
test -r /dev/fuse && test -w /dev/fuse
command -v fusermount3
./bin/ramet serve --config configs/fuse.yaml
```

Run the worker as your normal Unix user. If the device check fails, have the host
administrator grant that user access to `/dev/fuse`. The sample keeps snapshots
in `/tmp/ramet-fuse`; choose a persistent, private directory with `--data` for a
deployment. Keep snapshot sources outside that directory. Configure an API
token before exposing the worker beyond loopback; see [operations](operations.md).

Use the same Python or Go API as for copy/reflink. The README quick start's
snapshot, pool, promotion, and restore example works against this worker without
SDK changes. For durable snapshots after worker-disk loss, use the PostgreSQL/S3
configuration with `worker.backend: lazy`.

## Connect your runtime

On the worker host, tools running as the worker's Unix identity can use
`execution.path` directly. A custom sandbox adapter must attach **only that
execution's mount**, map the permitted identity, and stop processes and detach
its mounts before reset, release, discard, or shutdown. Keep the worker root and
sibling backing directories outside the sandbox.

The included Python Docker adapter currently supports copy/reflink workspaces;
FUSE mount propagation through that adapter is not supported. Use a native
worker for FUSE; the standard worker container image is for copy/reflink.
FUSE does not replace process isolation, a lease-loss supervisor, or runtime
resource limits. See the [integration guide](integration.md).

## What is lazy, and what is eager?

| Operation | Behavior |
| --- | --- |
| Acquire / warm | Prepare namespace and metadata without fetching working-tree file bytes. |
| First open / size or metadata change | Hydrate and verify the original file; concurrent first opens share the work. |
| Delete unopened file | Remove it without fetching its bytes. |
| Git import | Reconstruct Git metadata and optimized object packs eagerly. |
| Verify | Read and verify all files. |
| Promotion / preview | Read retained files; omitted ephemeral/rebuildable bytes need not be fetched. |
| Local fork | Produce an eager private copy of the current workspace. |
| Reset | Unmount the old tree and prepare a new mount from the base. |

A failed hydration returns an I/O error; a later open can retry after storage
recovers. Worker startup recovers abandoned Ramet mounts after a crash. Neither
uncommitted writes nor running processes survive worker failure. macOS and
`require_cow` cannot be combined with this backend; unsupported configurations
fail explicitly.

## Validate your host

From the repository root, with the prerequisites above:

```sh
RAMET_TEST_FUSE=1 go test -race -count=1 -timeout=3m -v -run TestLazy ./engine
make build
RAMET_TEST_BACKEND=lazy PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v
```

The engine suite checks lazy reads, mapped writes, truncation, permissions,
cross-execution isolation, Git preservation, selective persistence, reset, and
crash recovery. SDK tests exercise the HTTP worker lifecycle on FUSE; Docker
tests remain separate and skip unless explicitly enabled. For real storage
failure/retry and worker replacement, start the integration services and run
`RAMET_TEST_FUSE=1 ./scripts/test-integration.sh` as described in the
[developer guide](development.md).

For startup failures, check `/dev/fuse`, the helper executable, and worker
permissions. For a busy mount during cleanup, stop the runtime and release its
file handles and mount attachments before retrying. Do not force lifecycle
progress by deleting the workspace or ignoring an unmount error.

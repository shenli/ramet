# Operating the worker

## Start and configure

```sh
make build
RAMET_TOKEN=your-development-token ./bin/ramet serve \
  --data /var/lib/ramet --listen 127.0.0.1:7433
```

The data directory must be writable by the worker and private to its Unix user.
Put the directory on APFS, Btrfs, or an XFS filesystem with reflink support for
copy-on-write. `ramet doctor` probes the actual volume; filesystem names alone
are insufficient. `--require-cow` rejects materialization when cloning fails.

Configuration is intentionally small:

| Setting | Default | Purpose |
| --- | --- | --- |
| `--data` / `RAMET_DATA` | `.ramet` | Worker-private data directory |
| `--listen` / `RAMET_LISTEN` | `127.0.0.1:7433` | HTTP address |
| `RAMET_TOKEN` | unset | Bearer token; mandatory outside numeric loopback |
| `--require-cow` | false | Disallow full-copy fallback |
| `--config` | unset | Strict YAML configuration file |
| `--max-executions` | 4096 | Worker-wide active/reserved execution limit |
| `--max-pools` | 128 | Worker-wide pool limit |
| `RAMET_URL` (client) | `http://127.0.0.1:7433` | Client destination |

Do not put the data directory inside a snapshot source, or vice versa. Ramet
rejects overlap. Paths supplied through HTTP refer to the **worker's** filesystem.
Returned execution paths must be mounted by a colocated runtime; a remote SDK
does not transfer their contents to the client.

## Docker Compose

```sh
export RAMET_TOKEN="$(openssl rand -hex 32)"
docker compose up --build -d
./bin/ramet doctor
```

The image runs as UID 10001 and keeps state in the `ramet-data` volume. Port 7433
is published only on loopback. Tokens are configuration, never image layers.
To capture a local directory, add an explicit read-only bind mount and use its
container path in `snapshot --source`. Otherwise use `snapshot --git ...`.
The runtime consuming an execution must share the worker's execution volume and
mount only the chosen tree into its workload. Host Python `Execution.run()`
cannot directly use paths inside this container.

Docker Desktop and overlay-backed volumes may fall back to full copies. Measure
the backing volume with `doctor`; do not infer copy-on-write from successful
container startup. Keep API traffic on a private network, and terminate TLS
before crossing a host boundary.

## PostgreSQL and S3

The [reference configuration](../configs/postgres-s3.yaml) pairs PostgreSQL
metadata with S3-compatible immutable content. Both providers must be configured
together; the default remains local storage. YAML rejects unknown fields and
multiple documents. Secrets are read from named environment variables. Explicit
flags override YAML; `RAMET_DATA` and `RAMET_LISTEN` also override YAML.

```sh
export RAMET_TOKEN="$(openssl rand -hex 32)"
export RAMET_POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export RAMET_S3_ACCESS_KEY="ramet-admin"
export RAMET_S3_SECRET_KEY="$(openssl rand -hex 32)"
docker compose -f compose.remote.yaml up --build -d --wait
./bin/ramet doctor
```

Persist these credentials privately for subsequent starts. The reference stack
creates its own PostgreSQL database and MinIO bucket, stores their data in named
volumes, and exposes only the worker's loopback port. For external services,
create the bucket beforehand and set the endpoint, bucket, prefix, namespace,
and credential environment names in your YAML. Use TLS for off-host services.
The S3 endpoint is an HTTP(S) origin; credentials never belong in its URL.

One PostgreSQL session owns a namespace through an advisory lock. Use a direct
connection or session-affine proxy, not transaction pooling. Loss of that
connection fails durable operations and readiness; restart the worker after
service recovery. It does not reconnect and silently regain ownership. A
namespace is bound to its content endpoint/bucket/prefix to reject accidental
provider changes. Use a fresh cache directory when enabling remote storage.

Uploads complete before immutable manifests become visible in PostgreSQL.
Acquisition (copy) or first file open (lazy) hydrates missing content into a
verified bounded cache. Acquire/reset/recycle create no durable writes; eviction
can make later reads fetch remote content again. Pools and leases
remain local and ephemeral. A replacement worker can use the same configuration
and an empty cache after the previous namespace owner has stopped.

## Correctness and restore fidelity

- A workspace has one active writer, managed by the caller's runtime.
- Stop writers, terminate workload processes, and unmount old execution paths
  before reset, release, fork, checkpoint, or promotion. There is no process
  barrier or process-memory snapshot in this release.
- A published manifest is immutable and addressed by its canonical JSON hash.
  Referenced file bytes are hashed, synced, and linked into the private object
  store before manifest publication. File and directory sync errors are returned.
- File bytes, path namespace, regular permission bits, nanosecond mtimes, empty
  directories, and relative symlink targets are preserved and deeply verified.
  Root-directory metadata is worker-owned. Symlink metadata is not preserved.
  Relative parent links (such as `node_modules/.bin` links) are supported. Every
  symlink chain is resolved against the manifest namespace; absolute links,
  escapes above the workspace, cycles, and chains exceeding 40 links are rejected.
- Owner/group, ACLs, extended attributes, sparse extents, hardlink identity,
  setuid/setgid/sticky bits, sockets, device files, FIFOs, mounts, processes,
  logical clocks, and external side effects are outside this format's guarantee.
  Regular files must be readable and directories traversable by the worker.
  Special files are rejected during full capture or local fork. Ephemeral policy
  can omit them during promotion.
- Reset creates a new tree. The copy backend deeply verifies all supported
  state before return; lazy verifies the namespace and verifies bytes on open. Failure removes the slot from scheduling, increments the
  quarantine counter, and destroys suspect local state. Diagnostics are returned
  in the error; quarantine does not retain a forensic tree.
- Every acquisition rotates an unguessable lease. Reset advances the generation
  and changes the path. Both are checked by every execution operation. Old file
  descriptors continue to refer to an unlinked old tree. External sandbox
  integration is still required to prevent a process from reopening new paths.
- Working-tree Git metadata is independent per execution. Optimized imports
  preserve logical Git objects in shared base and local-only packs, retaining
  refs/reflogs/index and staged/unstaged state. Physical `.git/objects` layout
  is normalized; both packs are durable and restore without the original remote.
  Git sources are cloned without hardlink sharing and pinned to a commit.
  Submodules are rejected by Git import; hydrate them in a directory first.
  LFS hydration is the caller's responsibility. Bare repositories and linked
  worktrees must be converted into standalone working trees before capture.
- Snapshot capture checks for ordinary file changes during its read, but this
  is a diagnostic, not an atomic barrier against arbitrary concurrent writers.

Initial snapshot and checkpoint capture all supported files. **Only promotion**
applies the policy exclusions. No secret detection is performed.

## Persistence policy

See [policy preview and verified reconstruction](persistence.md) for per-path
inspection, recorded exclusions, and input-checked restore plans.

Save this JSON policy and pass it with `ramet snapshot --policy policy.json`:

```json
{
  "rules": [
    {"path": "node_modules", "class": "rebuildable", "recipe": "npm ci"},
    {"path": "tmp", "class": "ephemeral"},
    {"path": "toolchain", "class": "shared_immutable"}
  ]
}
```

Rules match a relative path and descendants. The longest prefix wins; no globs
or implicit dependency-directory exclusions are used. `.git` cannot be excluded.
Unknown paths are durable. Rebuildable rules require a nonempty recipe; the
manifest records it for your runtime to execute when needed. A promoted snapshot
omits excluded state, including excluded base bytes. Durable descendant rules
retain their parent directories. Shared-immutable mutations or deletions cause
promotion to fail instead of silently discarding them.

## API and CLI

The JSON API is versioned under `/v1`. Request bodies are limited to 1 MiB,
unknown fields are rejected, and worker operations have a two-minute deadline.
The Go/Python HTTP clients default to a slightly longer response timeout. There
is no automatic mutation retry: a timed-out response may have committed.

| Method | Path | Request / result |
| --- | --- | --- |
| POST | `/v1/snapshots` | `{source, policy}` or `{git: {repository, revision}, policy}` → snapshot |
| GET | `/v1/snapshots` | Snapshot list |
| GET / DELETE | `/v1/snapshots/{id}` | Read / delete unreferenced snapshot |
| GET | `/v1/snapshots/{id}/restore-plan` | Recorded save mode and reconstruction steps; no execution or readiness guarantee |
| GET | `/v1/snapshots/{id}/locality` | Unique object/byte residency and idle execution count |
| POST | `/v1/snapshots/{id}/prefetch` | No body; hydrate and verify snapshot content |
| POST | `/v1/pools/{pool}/warm` | `{count}` → newly prepared and current idle counts |
| POST | `/v1/pools` | `{base, concurrency, lease_seconds}` → pool; concurrency 1–10,000 |
| GET / DELETE | `/v1/pools/{pool}` | Read / close empty pool |
| POST | `/v1/pools/{pool}/acquire` | No body; waits for capacity → execution |
| POST | `/v1/pools/{pool}/acquire-many` | `{count}` → execution array; all-or-nothing reservation |
| POST | `/v1/pools/{pool}/executions/{id}/{action}` | `{lease, generation}` |
| GET | `/v1/doctor` | Deep content/lineage check and COW probe |
| POST | `/v1/gc` | `{grace_seconds: 3600, dry_run: true}` → eligible object/byte count |
| GET | `/v1/worker/cache` | Cache bytes, pins, headroom, idle limits and pressure counters |
| POST | `/v1/worker/cache/trim` | `{target_bytes: 0}` → remote cache report; pinned objects remain protected |
| POST | `/v1/worker/idle/trim` | No body; remove all idle trees → `{removed}` |
| POST | `/v1/worker/drain` | Stop accepting claims, forks, and new pools |

Execution actions are `reset`, `fork`, `verify`, `checkpoint`, `promote`,
`heartbeat`, `release`, and `discard`. Release/discard and successful deletes return 204;
other successes return JSON with status 200. Errors have `{code, error, request_id}` and
statuses 400 (invalid), 401 (unauthorized), 404 (missing), 408 (deadline),
409 (conflict/stale or expired lease), 413 (body too large), 422 (verification),
429 (worker quota), or 503
(draining). Unexpected I/O errors return 500 and are logged at the worker.

Example CLI lifecycle (requires `jq`):

```sh
./bin/ramet snapshot --source /absolute/path/to/source > snapshot.json
./bin/ramet pool --base "$(jq -r .id snapshot.json)" --concurrency 8 > pool.json
./bin/ramet acquire --pool "$(jq -r .id pool.json)" > execution.json
# Mount the path in execution.json into your runtime and run an attempt.
# Stop/unmount that runtime before continuing.
./bin/ramet promote --execution execution.json > promoted.json
./bin/ramet reset --execution execution.json > reset-execution.json
./bin/ramet release --execution reset-execution.json
./bin/ramet close-pool --pool "$(jq -r .id pool.json)"
```

Execution JSON contains a lease token. Keep it private and replace it after a
reset. Do not redirect reset output into its input file: the shell truncates it
before the command can read it. Python handles path/generation replacement for
you; the Go API returns the replacement value.

Execution actions also include `preview-promotion` and `restore-plan` at
`POST /v1/pools/{pool}/executions/{execution}/{action}`, with the same
`lease`/`generation` request as other execution actions. Preview returns policy
decisions and violations without publication. The execution restore plan checks
current declared input hashes; the snapshot route only reads the recorded plan.

## Restart, drain, and failures

`/live` reports that the HTTP handler is serving. `/ready` reports initialization
and drain status, plus remote provider reachability when configured. `ramet doctor` is a separate, potentially expensive integrity
check; readiness is not a deep scan on every poll. `/metrics` exposes
Prometheus counters for acquisition, reset/recycle, verification failures,
quarantine, expired leases, cloned/copied files, local content writes, and remote
bytes uploaded/downloaded, plus gauges for active/idle
executions and pools. HTTP latency histograms use bounded route-template and
outcome labels. Cache metrics expose bytes, evictions, hits/misses and pressure;
lazy metrics count hydrated files, hydration errors and total hydration time.
Cleanup errors increment `ramet_maintenance_errors_total`. Error logs carry
request ID, tenant, route, pool/execution IDs where available, and outcome;
responses include `X-Request-ID`. Leases and bearer credentials are not logged.

`ramet drain` rejects new acquisitions/forks/pools while existing callers may
finish, promote, release, or discard. Drain remains in force until worker restart.
Wait for `ramet_active_executions` to reach zero, close pools, and stop the worker.
SIGTERM drains, allows up to 15 seconds for HTTP requests, and discards ephemeral
state. It does **not** wait for an external sandbox to stop or checkpoint itself.

Only one worker process can open a data directory, enforced by a nonblocking
OS file lock. On restart, Ramet removes staging files and execution directories
left by the previous process. It retains immutable content and manifests. If the
old sandbox is still running, stop it before restarting the worker.

Leases default to 900 seconds; set pool `lease_seconds` or CLI `--lease-seconds`
from 1 to 86,400. Renew with `heartbeat` before expiry. Python execution contexts
and `RametEnv` renew automatically, including during `Execution.run()`. Go clients
and manually held Python executions must arrange renewal themselves. Reset
renews the lease. Expired leases cannot be resurrected; a one-second reaper
quarantines and deletes abandoned slots instead of recycling them. Operations
already in progress finish under their slot lock before reclamation. The runtime
must stop processes when ownership is lost; Ramet does not kill external workloads.

Use `ramet acquire --pool ID --count N` for an atomic batch. If construction
fails, the entire cohort is reclaimed. Batch waiting never holds a partial
reservation. Worker limits bound active/reserved slots across all pools; idle
trees have a separate count/TTL budget. Remote cache bytes and managed disk
headroom can also be bounded in YAML (below).

## Garbage collection and backups

`ramet gc` is a dry run with a one-hour grace period. `ramet gc --run` removes
eligible unreachable **local cache** content. Remote S3 objects are never deleted
by worker GC. It holds an exclusive content lock against
snapshot/promotion/materialization and marks every durable manifest. Corrupt or
unreadable manifests abort collection. Failed captures may leave safe orphan
objects; no manifest is published for a partially captured tree.

Deleting a snapshot fails if it is a pool base or another snapshot's parent.
Delete descendants first. Remote-cache LRU eviction is independent of snapshot
deletion: it can evict referenced bytes because remote storage remains
authoritative. Automatic durable snapshot/remote-object retention is not enabled.

For backup, drain and stop the worker, then copy `FORMAT`, `snapshots/`, and
`objects/` together. Staging, executions, and `LOCK` are disposable. Restore those
durable files into a private directory, start the worker, and run `ramet doctor`
before acquiring from restored snapshots. Online backup requires external
filesystem snapshot coordination; copying a mutating directory is not a supported
backup procedure.

For remote storage, back up PostgreSQL and retain every referenced S3 object.
Drain and stop publication while taking a coordinated backup. Restore metadata
and content before starting a worker with an empty cache, then run `doctor`.
Do not restore only PostgreSQL after independently deleting bucket objects.
Cross-worker remote object collection and automated retention are deferred.

## Format and upgrades

The store layout marker remains version 1. New immutable manifests use version 4
for large-file chunk lists, retaining save reports and reconstruction contracts;
versions 1 through 4 are readable. Older binaries cannot read version 4 snapshots;
do not downgrade a store after
creating them. Unsupported versions fail
closed before recovery cleanup. PostgreSQL schema version 1 is initialized by a
transactional migration on startup, serialized by an advisory lock. Run
`ramet migrate --config config.yaml` while the namespace has no worker owner to
apply/check the supported schema explicitly. Newer schemas are rejected; no
schema downgrade or destructive migration is performed. Future changes must provide
explicit compatibility/migration tests. Back up before upgrading. Server/SDK
version skew beyond the documented `/v1` fields has no stability guarantee while
the project is alpha.

## Prepare workers before a workload

```sh
./bin/ramet prefetch --id SNAPSHOT_ID
./bin/ramet locality --id SNAPSHOT_ID
./bin/ramet warm --pool POOL_ID --count 16
```

Prefetch fills and verifies immutable content without creating execution trees.
Warm prepares enough verified idle trees to reach the requested count at
reservation time. It creates no leases and writes no durable state. Acquire
verifies a warm tree again according to its backend contract. Warming reserves pool and worker
capacity during construction; an occupied pool or concurrent preparation can
return conflict, and the worker-wide limit can return quota exceeded. Warm
fails during drain and a pool cannot close while preparation is in progress.

The result's idle count is a point-in-time value: concurrent acquisitions may
consume prepared trees immediately. Locality counts unique object bytes, checks
file type/size, and reports idle executions for the base; it does not hash files
or reserve residency. Prefetch does hash files and rejects corrupted cache bytes.
A cancelled prefetch may leave verified objects cached. Failed warm construction
removes its newly built cohort without returning partial usable trees.

Idle trees expire and obey a per-store count limit. Ramet does not automatically
replenish a warm target. Prefetch can cycle through a cache smaller than the base:
success means every object was verified, not that every object remains resident.
Warming a lazy pool prepares namespaces; use prefetch separately to fetch bytes.
Metrics include `ramet_warmed_executions_total`, `ramet_prefetches_total`, and
`ramet_preparing_executions`.


## Cache and admission limits

Add these fields to your worker YAML; see [remote example](../configs/postgres-s3.yaml):

```yaml
server:
  max_inflight: 128
worker:
  backend: copy
  disable_chunking: false
  cache_bytes: 1073741824
  min_free_disk_bytes: 1073741824
  max_idle_executions: 32
  idle_ttl: 5m
```

`disable_chunking` defaults to false. Set it to true to save new captures as
whole-file objects when comparing latency or favoring repeated local reflink
materialization. Existing chunked snapshots remain readable. See
[artifact checkpoints](artifact-checkpoints.md) for the storage/restore tradeoff.

`cache_bytes` is remote-mode only (default 0, unlimited). It bounds ready and
in-flight cache objects; an individual object must fit. LRU eviction protects
pinned readers and never removes authoritative local objects or remote data.
`ramet trim-cache --target-bytes 0` evicts unpinned remote cache; it can return
429 while objects are pinned. It does not discard execution files already read.

Idle defaults are 128 trees and five minutes. Counts include warming reservations.
Release destroys its newly reset tree if the idle limit is full. `ramet trim-idle`
removes idle trees immediately. Active/reserved execution quotas remain separate.

`min_free_disk_bytes` defaults to 0. Reservations protect Ramet-managed data
copies and hydration before allocating bytes. This is not a filesystem quota
for arbitrary workload writes, filesystem metadata or an external Git clone.
Use volume/project quotas and runtime limits to enforce those hard boundaries.
Free-space races with unrelated host processes remain possible; I/O errors fail
operations rather than publishing partial success.

`max_inflight` defaults to 128 per tenant/worker. Excess requests get 429 with
`Retry-After: 1`; health, metrics and lease heartbeats remain available. Clients
do not automatically retry mutations with potentially ambiguous outcomes.

## Lazy hydration

See the [Linux FUSE guide](fuse.md) for setup commands, a sample configuration,
runtime attachment requirements, and validation checks.

Set `worker.backend: lazy` on a native Linux worker with accessible `/dev/fuse`
and `fusermount3` (or `fusermount`) installed. The copy backend is the default on
all platforms; selecting lazy on macOS fails explicitly. Lazy and `require_cow`
cannot be combined. The standard container image is intended for the copy
backend; use a host worker for the tested FUSE deployment.

Acquisition exposes complete namespace metadata before fetching working-tree
file bytes. Opening a file, or changing its metadata/size, hydrates and verifies
its original content into a private inode first. Concurrent first reads share
that work. Read failures return I/O errors; retry after provider recovery can
hydrate again. Git metadata and optimized object packs are reconstructed eagerly.

Explicit `verify` reads all files. Capture reads retained files, and a local fork
currently returns an eager copy. Warm lazy slots remain lazy until accessed.
Unopened file deletion does not fetch bytes. Reset unmounts the old tree and
creates another mount/namespace. The runtime must release its mounts and stop
writers before lifecycle operations, including shutdown or worker replacement.
Startup unmounts abandoned Ramet FUSE mounts before removing ephemeral trees.

The FUSE mount is accessible to the mounting Unix identity. A colocated sandbox
must map that identity appropriately and mount only the selected execution;
never expose its sibling backing directory or the worker's root. This backend
is not an independent sandbox or a process-state snapshot.

## Tenant gateway

Start `ramet serve --config configs/tenants.yaml` after adapting the example
paths and providing each tenant's `server.token_env`. Each tenant config selects
its own data root, metadata namespace, content prefix and capacity limits. The
gateway rejects duplicate tokens, overlapping roots and shared durable namespace/
content identities. Relative tenant config filenames resolve beside the gateway
file; worker/source paths should be absolute.

Gateway source capture is restricted to `worker.source_roots`; Git import is
restricted to exact `worker.git_repositories` entries. Empty lists deny those
operations. Source symlinks are resolved before checking the allowlist. Do not
allow broad host directories: allowlists must not overlap any tenant store.
Single-worker deployments can enable the same restriction with
`worker.restrict_sources: true`.

Bearer credentials select the tenant. No tenant header or URL parameter can
change it. Snapshot, pool, lease, cache/GC, doctor, metrics and drain operations
apply only to that store. `/live` is public; `/ready` and all other gateway
routes require a tenant credential. Graceful process shutdown drains all stores.

This is API/state and managed-capacity isolation. For hard control-plane CPU and
memory limits, deploy tenants in separate worker containers/processes. External
sandboxes must additionally enforce workload CPU, memory, process, mount and
network boundaries. `scripts/phase2-soak.py` includes a real Docker check of a
single-execution mount with those restrictions. Neither API tokens nor FUSE
replace that runtime boundary.


## Operation receipts

Optional `Idempotency-Key` headers identify mutations. Obtain an ID from
`GET /v1/operations/new` before issuing the request. Inspect it through
`GET /v1/operations/{id}`; `GET /v1/capabilities` reports support and limits.
Identical method/path/body retries return the recorded response, including an
original error. Different request bytes return `operation_conflict`.

Receipts are scoped to the credential-selected worker API session and expire one
hour after issuance. They are in memory; worker restart/replacement invalidates
all old IDs and never reexecutes their requests. Historical receipts do not
renew execution leases or establish readiness. Capacity is bounded and reserved
before execution; oversized responses leave an unknown-result receipt. Read the
[recovery decision table and storage limits](agents.md#recovery-decisions).
Unkeyed requests are unaffected and still must not be blindly retried.

CLI: `operation-id`, `operation --id ID`, and `--operation-id ID` on a mutation.
`--json-errors` emits structured failures on stderr. SDK errors expose request
IDs for correlation; API 404/405 errors use the same JSON envelope as application
errors. Policy files can be checked locally with `policy-validate --policy FILE`.

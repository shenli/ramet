# Implementation design

This guide explains the Phase 2 state model, failure behavior, and tradeoffs.
It complements the [architecture](architecture.md) and is the starting point for
changes to lifecycle or storage semantics. The [product design](ramet-product-technical-design.md)
contains broader proposals; it does not override the current implementation's
[restore contract](operations.md#correctness-and-restore-fidelity).

## Core model

| Model | Meaning | Lifetime |
| --- | --- | --- |
| `Entry` | Relative path, kind, permission bits, mtime, and file digest/size or symlink target | Part of a manifest |
| `Manifest` | Format version, parent, Git provenance, path policy, sorted entries | Immutable after publication |
| `Snapshot` | Manifest and SHA-256 ID | Durable until explicitly deleted |
| `Pool` | One base snapshot, concurrency limit, lease duration, active and idle slots | Current worker process |
| `Execution` | Slot ID, pool/base IDs, path, lease, generation, expiration | One active claim |

A manifest is a full tree description. The parent is lineage, not a delta to
apply. Two captures have the same ID only if their serialized manifests match,
including metadata, policy, provenance, and parent. This is not Git's object
identity scheme. Changing manifest serialization can change snapshot IDs and
must be treated as a compatibility decision.

A checkpoint or promotion is a child of the **pool base**. Capturing an execution
does not advance the pool base or create a journal of all previous captures.
To resume a selected result, create a new pool using the returned snapshot.

## Invariants

1. Published manifests refer only to content whose durable write succeeded.
2. Writable executions never share a writable inode with content objects or
   other executions. Reflinks may share extents with copy-on-write semantics.
3. Availability follows backend verification: full bytes for copy; namespace first
   and verified bytes before file access for lazy.
4. Failed reset or acquisition never returns a suspect slot to the idle set.
5. Every execution operation checks the active claim's lease and generation.
6. Expired leases cannot be revived, and abandoned trees are destroyed.
7. Failure or cancellation during batch construction releases the whole cohort.
8. Remote metadata operations require the original exclusive ownership session.

These invariants are more important than maximizing a benchmark's throughput.
A performance change must preserve them or explicitly revise the supported
contract, its documentation, and its failure tests.

## Capture and publication

Capture walks a quiesced directory, validates paths, hashes regular files, and
writes immutable content through staged files. Local writes sync before
publication. Regular files are checked for ordinary changes during capture;
this detects some races but is not a transactional barrier against concurrent
writers. The caller must stop writers.

Entries are sorted by path and serialized using the Go manifest model. The
JSON representation requires valid UTF-8 for paths, symlink targets, policy
recipes, and Git provenance. Capture rejects unsupported byte sequences before
publication so serialization cannot silently rename files or rewrite targets.
Valid Unicode text is preserved without normalization.

The manifest's SHA-256 becomes its ID. Local publication uses an atomic write. In
remote mode, `publishSnapshot` first uploads unique referenced objects, then
calls `Metadata.Publish`. It never reverses that ordering.

| Failure point | Visible result | Recovery |
| --- | --- | --- |
| Local capture fails | No new successful snapshot result; local orphan objects may remain | Later local GC can reclaim unreachable content |
| Remote upload fails | No metadata publication for that capture | Retry capture after fixing the provider; remote orphans are retained |
| Metadata publication fails | Caller receives an error | Some content may already exist; inspect state before assuming absence |
| Response is lost after publication | Outcome is ambiguous to the caller | List/read snapshots or repeat an identical immutable publication where appropriate |
| Local cache is lost after successful remote publication | Durable snapshot remains | Open a replacement worker with the same providers and an empty cache |

Immutable provider publication is idempotent for the same ID and bytes. That
does not make every API mutation safe to retry: a lost acquire response may
leave a live lease, and release/reset can invalidate the old handle. Clients do
not automatically retry general mutations.

The S3 provider checks upload input digest and size and validates an existing
object before treating it as deduplicated. It rejects corrupted existing bytes.
Downloads stage, hash, size-check, sync, and link into the local cache. The remote
content interface intentionally has no delete method; safe shared remote
retention is outside this milestone.

## Acquire and batch reservation

`Acquire` delegates to `AcquireMany(ctx, 1)`. Batch counts must fit pool capacity.
If the pool lacks capacity, callers wait on its change notification and honor
context cancellation. They hold no partial reservation while waiting. When
capacity is available, the worker atomically reserves the entire cohort against
the global active/reserved execution limit. Exhausting that global limit fails
promptly with `ErrLimit` rather than waiting across pools.

The pool moves idle slots or allocates new slots into the active map, rotates
leases, and increments generations. Construction uses up to 16 goroutines per
batch outside the pool scheduling lock. The copy backend materializes and deeply verifies new trees, and verifies idle
trees again. Lazy trees receive namespace verification; file content is verified
on first open. Any construction failure cancels the
remaining work and quarantines every reserved slot. No partial result is usable
when the batch returns an error. Successful leases receive their expiration
when construction completes.

The quota counts active/reserved executions, including forks, not idle cached
trees or disk bytes. Pool limits also apply worker-wide. Separate idle-count, idle-age and remote-cache budgets apply to each store.
Gateway tenant stores have independent budgets; sandboxes enforce workload
CPU, memory, process and network limits.

## Slot transitions

| From | Operation | Successful transition |
| --- | --- | --- |
| No slot or idle slot | Acquire | Verified active execution with a new lease and generation |
| Active | Heartbeat | Same path/generation, later expiration |
| Active | Reset | New verified base tree and generation; lease renewed |
| Active | Release | Reset to base, clear lease, return slot to idle, release quota |
| Active | Discard | Remove from scheduling, delete tree, release quota |
| Active | Fork | Parent unchanged; additional active child contains current local state |
| Active | Checkpoint / promote | Publish snapshot; execution remains active |
| Reserved or active | Construction/reset failure or expiry | Quarantine, remove from scheduling, attempt tree cleanup |
| Idle | Pool close | Delete cached tree |

Quarantine records failure through errors and metrics; it does not retain a
forensic workspace. Cleanup errors are returned, but a suspect slot remains
unschedulable even if deleting its directory fails.

Reset creates a new directory identity before removing the old tree. This keeps
old open descriptors attached to the old inode rather than the next execution.
It is more expensive than an overlay pointer swap and still needs external
process isolation. A stale privileged process could reopen the new path if the
runtime allows it.

Explicit `Verify` compares an active execution against its base and can report
expected user modifications. Such differences alone do not quarantine active
work. Verification failure during acquire/reset does quarantine the slot.

## Lease and concurrency design

Leases default to 900 seconds and can range from 1 to 86,400 seconds. The worker
checks expiry while claiming a slot. A one-second background reaper attempts to
lock each active slot and removes expired ones. It skips busy slots; an operation
already holding the slot lock is not interrupted midway through publication or
reset. Expiration is therefore not a hard process execution deadline.

| Coordination primitive | Protected responsibility |
| --- | --- |
| `Store.life` read/write lock | Keep operations alive while close waits and tears down the worker |
| `Store.contentMu` | Exclude GC/deletion from conflicting capture and materialization |
| `Store.mu` | Pool registry and drain state |
| `Pool.mu` | Scheduling maps, idle list, close state, and change notifications |
| Slot mutex | One lifecycle operation per execution; claim validation and mutation |
| Atomic allocation counter | Worker-wide cohort/fork capacity reservation |
| `Store.cacheMu` and per-object ready channels | Pin readers, coalesce same-object fetches and coordinate LRU eviction |
| `Store.diskMu` | Reserve disk headroom before managed data writes |
| Lazy mount mutex | Coordinate hydration with rename, link, unlink and other file mutations |
| PostgreSQL provider mutex | Serialize use of the single ownership connection |

When modifying locks, inspect the whole call path, including quarantine, close,
and reaping. Keep filesystem and network work outside scheduling locks. Preserve
existing lock acquisition order; do not add a reverse acquisition while holding
a slot or content lock. Add a race-enabled concurrency regression for changes
that affect reservation, release, drain, or shutdown.

Python execution contexts renew leases in a background thread. A separate state
lock allows renewal while a trusted `run()` holds the lifecycle lock. Fatal
missing/stale-handle responses stop renewal; the SDK does not terminate an
external runtime on heartbeat failure. Go callers and Python handles outside a
context must arrange renewal themselves.

## Persistence policy and Git

Promotion uses longest-prefix path rules, defaulting unknown paths to durable.
Initial snapshots and checkpoints capture all supported files regardless of
promotion exclusions. [Persistence inspection and reconstruction](persistence.md)
describes advisory previews, recorded save modes/exclusions, recipe input hashes,
and caller-approved restore callbacks. Previews publish no content; actual
promotion independently rescans the quiesced execution.

| Class | Promotion behavior |
| --- | --- |
| `durable` | Retain current state |
| `ephemeral` | Omit matching state, except parents needed by retained descendants |
| `rebuildable` | Omit bytes and retain the required reconstruction recipe |
| `shared_immutable` | Retain unchanged base state; reject additions, edits, or deletion |

Recipes are metadata for the runtime; Ramet does not execute them. `.git` cannot
be excluded. Git import disables hooks and ambient repository configuration,
clones without local hardlink sharing, resolves the requested revision to a
commit, and checks it out detached. Executions retain independent Git metadata,
including local-only commits and refs. Submodules require caller hydration;
LFS bytes must also be hydrated by the caller.

Git imports establish a trusted object inventory. The manifest retains a shared
base pack and its sorted inventory in Ramet CAS. Subsequent capture packs all
objects absent from that original inventory, including unreachable objects,
staged blobs and local-only history. Changing a remote-tracking ref cannot
reclassify an object as externally durable. Physical loose/pack layout is
normalized; refs, reflogs, index, config and working-tree metadata remain durable.

Both base and local packs are retained in the configured durable provider.
Restoration clones or copies each pack into an independent inode, generates its
Git index with `index-pack`, compares the full logical object inventory, and
runs `git fsck`; no Git remote fetch is required. Repacking produces the same
canonical local pack. Pruning a required base object makes capture fail with a
conflict instead of silently resurrecting it. Capture a fresh Git base to change
that provenance boundary. Shallow/alternate-backed repositories are rejected.
Directory-only snapshots retain their literal `.git` layout; import with `Git`
to establish the optimized provenance model.

## Recovery, migration, and compatibility

The filesystem store marker remains version 1. New manifests use version 3 for
save reports and reconstruction contracts; readers accept versions 1, 2, and 3. Unsupported versions
fail before recovery cleanup. PostgreSQL schema version 1 is initialized in a
transaction guarded by a database-wide migration lock. Newer schema versions
are rejected; `ramet migrate --config ...` exposes this migration explicitly.
It also claims namespace ownership, so stop that namespace's worker first.

Startup takes the local lock, validates storage configuration and provider
binding, then removes abandoned execution/staging trees and unpublished local
manifest temporary files. It retains immutable published state. Remote metadata
connection loss fails durable operations closed; there is no automatic reconnect
that could bypass namespace ownership.

Local GC marks all durable manifests and observes a grace period before deleting
unreferenced cached content. Snapshot deletion rejects pool bases and referenced
parents. Remote S3 objects are retained after metadata deletion. Cache eviction may
remove referenced local objects only in remote mode; it never deletes remote data.

For future format changes, define readable versions, migration ordering,
interrupted-migration recovery, and rollback limitations before changing the
writer. API versioning, filesystem format, PostgreSQL schema, and SDK package
versions are distinct compatibility surfaces.

## Why this design

The local execution path avoids durable branches for disposable attempts.
Complete manifests simplify reconstruction and verification. Full-tree reset
and deep verification favor a clear correctness contract at the cost of I/O
proportional to file count and bytes. A single metadata ownership session makes
worker replacement explicit while avoiding a distributed coordination protocol.

Phase 2 keeps those durability boundaries while making cache residency and
namespace readiness explicit. Local authoritative objects are never pressure-
evicted. A remote object becomes evictable only after its upload succeeds.
Readers pin before opening paths, and downloads reserve cache bytes before
fetching. Concurrent readers of one missing digest share a ready notification.
Objects larger than the cache budget fail admission; a tree larger than the
budget can stream through it one object at a time. Prefetch verifies every
object but cannot promise all objects remain resident in an undersized cache.

Idle reservations count warming slots as well as completed idle trees. Release
resets first, then retains the clean tree only if the idle budget permits it.
The reaper removes old idle trees; explicit trimming never deletes active trees.
Background cleanup failures increment a maintenance counter. GC marks all
manifest references, including Git packs/indexes, and fails closed when metadata
cannot be read. Pins also protect FUSE readers that outlive an HTTP request.

The lazy backend builds sparse private placeholder inodes and exposes their
manifest metadata through FUSE. It hydrates into the same inode before open or
metadata mutation, restoring permissions and nanosecond mtime. Hard links and
renames preserve inode identity; unopened unlink avoids fetching content.
Hydration is bounded by mount lifetime and a timeout. Kernel-request
interruptions do not cancel it: runtime signals can interrupt an open that the
caller immediately retries. Provider failures return I/O errors and leave the
inode pending; no placeholder zeros are exposed as successful content.

Explicit verification and capture read file contents. Fork currently materializes
an eager private copy. Reset/release unmount the old lazy tree and create a new
one; startup identifies and unmounts only owned `fuse.ramet` mounts before crash
cleanup. FUSE does not supply process isolation, and callers must stop and
unmount their workloads before lifecycle transitions.

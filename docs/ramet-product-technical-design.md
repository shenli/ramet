# Ramet: Product and Technical Design

**Status:** Draft  
**Audience:** Contributors, users, infrastructure engineers, and platform developers  
**Primary implementation language:** Go

> Ramet is state infrastructure for persistent and parallel AI agents.

This document describes the product model, APIs, architecture, correctness semantics, deployment model, implementation plan, and benchmarks. It intentionally avoids internal marketing strategy and launch positioning.
## 1. Executive Summary

Ramet is open-source state infrastructure for AI agents.

The long-term goal is to provide a common state substrate for two important classes of workloads. The initial OSS wedge focuses on the second class:

1. **Production agents**, where each execution matters and the system must optimize for low latency, durability, predictable tail behavior, and fast resume.
2. **Parallel agent workloads**, including RL and evaluation, where the system must optimize for sustained throughput, cheap reset and reuse, aggressive sharing, and low cost per execution.

These workloads need many of the same state primitives, but they should not be forced through the same hot path.

Ramet is built around four ideas:

1. **Share immutable state aggressively.**  
   Container images, Git repository bases, toolchains, dependency bundles, models, and datasets should be referenced and cached rather than copied per sandbox.

2. **Isolate only mutable execution state.**  
   Each execution should maintain a small copy-on-write delta over shared state.

3. **Persist according to recoverability semantics.**  
   Irreproducible state is durable; reproducible state is rebuildable; temporary state is ephemeral.

4. **Make state lifecycle explicit.**  
   `snapshot`, `fork`, `reset`, `recycle`, `checkpoint`, `verify`, `discard`, `promote`, and `resume` are first-class operations.

The high-level architecture is:

```text
                         Application APIs

                  Workspace      ExecutionPool
                      \             /
                       \           /
                        Lifecycle Core
                              |
                 Snapshot / Restore / Verify
                              |
          +-------------------+-------------------+
          |                                       |
      Durable State                         Local Execution
          |                                       |
   StateStore + ContentStore              ExecutionStore + Cache
          |                                       |
 versioned metadata + immutable data        local COW delta
```

Native database branching is an **optional provider acceleration**, not part of the correctness model.

Ramet is not intended to be a general-purpose distributed filesystem. It is a state system optimized for agent environments.

---


## 2. Product Scope

Ramet provides state lifecycle primitives for agent environments:

```text
snapshot
fork
reset
recycle
checkpoint
verify
discard
promote
resume
```

The system supports two execution profiles:

- **Production agents**, optimized for latency, durability, recovery, and predictable tail behavior.
- **Parallel workloads**, including RL and evaluation, optimized for throughput, sharing, reset/reuse, and low cost per execution.

The first optimized state profile is **Git-aware coding environments**, because they provide a realistic workload with large shared immutable state, rebuildable dependencies, and relatively small mutable deltas.

Ramet is not intended to be:

- a general-purpose distributed filesystem,
- a complete RL framework,
- a general sandbox scheduler,
- a transparent rollback system for arbitrary external side effects,
- or a general distributed multi-writer storage system.

The initial consistency model assumes one active writer per execution or workspace branch.

## 3. Users, Workloads, and SLOs

Production and parallel workloads share state semantics but optimize different objective functions.

| Dimension | Production Agent | Parallel / RL / Eval |
|---|---|---|
| Primary objective | Low latency + reliability | Sustained throughput + cost efficiency |
| Unit of value | Each execution matters | Population of executions matters |
| Latency | Direct product SLO | Important when it limits throughput |
| Tail behavior | p95/p99 affects users | Important for barriers and stragglers |
| Durability | Strong | Often weak for an individual execution |
| Failure handling | Recover and resume | Isolate, retry, or discard |
| State lifetime | Hours to months | Often seconds to minutes |
| Fan-out | Low to moderate | Potentially massive |
| Default state policy | Durable | Ephemeral |
| Core efficiency metric | latency / reliability per execution | successful executions / second / dollar |

A useful summary is:

> **Production optimizes the latency and reliability of one important execution.**

> **Parallel workloads optimize the throughput and cost of a large population of disposable executions.**

### 3.1 Production Agent Profile

Typical examples:

- coding agents,
- research agents,
- workflow agents,
- browser or computer-use agents,
- long-running background agents,
- enterprise agent platforms.

Important properties:

- low mount and resume latency,
- predictable p95/p99 latency,
- durable-by-default behavior,
- bounded durability lag,
- crash recovery,
- reliable checkpointing,
- warm-cache performance,
- branch and rollback support.

A worker loss is a recovery event, not permission to lose user state.

### 3.2 Parallel / RL / Evaluation Profile

Typical properties:

- high fan-out,
- short-lived executions,
- mostly single-writer sandboxes,
- large discard ratios,
- repeated reset to the same base state,
- bursty creation and destruction,
- high sensitivity to sustained throughput and cost,
- strong need for deterministic reset,
- tolerance for retrying an isolated failed execution.

Single-operation latency is secondary unless it reduces aggregate throughput.

Long-running stability matters more than benchmark peaks. The system should not degrade after hundreds of thousands or millions of execution lifecycles because of leaked mounts, cache churn, GC backlog, stale state, or cleanup failures.

---

## 4. Developer Experience

Ramet exposes user concepts rather than storage mechanisms.

There are three API layers:

```text
Production Developers
        |
     Workspace
        |
RL / Eval Developers
        |
   ExecutionPool
        |
--------------------------------
Ramet Internals
--------------------------------
        |
   Lifecycle Core
        |
    Provider SPI
  /      |       \
State  Content  Execution
Store   Store     Store
```

`StateStore`, `ContentStore`, native branching, and overlay implementation are provider concerns, not normal user-facing concepts.

### 4.1 Production DX: `Workspace`

A production agent platform developer usually wants:

> Give this agent a durable workspace, let it survive sandbox replacement, and let me resume or fork it later.

A Go SDK should make that simple:

```go
client := ramet.NewClient(...)

ws, err := client.Workspaces.Create(ctx, ramet.CreateWorkspaceRequest{
    Base: ramet.GitBase{
        Repository: "https://example.com/org/repo",
        Revision:   "main",
    },
    Profile: ramet.ProfileProduction,
})
if err != nil {
    return err
}

_, err = ws.Mount(ctx, ramet.MountOptions{
    Target: "/workspace",
})
```

The agent sees:

```text
/workspace
```

and can use normal tools:

```text
git status
grep
go test ./...
npm test
```

A typical lifecycle is:

```text
create workspace
      |
mount in sandbox A
      |
agent modifies state
      |
checkpoint
      |
sandbox A disappears
      |
create sandbox B
      |
resume workspace
```

The application developer should not need to know:

- which metadata backend is configured,
- which object store is used,
- whether the provider supports native branching,
- whether the local engine uses OverlayFS or reflinks,
- how writeback is scheduled.

### 4.2 Parallel / RL / Eval DX: `ExecutionPool`

An RL or evaluation developer usually wants:

> Give me many clean executions starting from this state, and make reset and reuse efficient.

Python should be a first-class SDK for this audience.

```python
from ramet import Ramet

store = Ramet()

base = store.snapshot(
    source="git://example.com/org/repo@abc123"
)

pool = store.execution_pool(
    base=base,
    profile="parallel",
    concurrency=1024,
    verify_on_reset=True,
)
```

Acquire one execution:

```python
with pool.acquire() as execution:
    execution.exec("pytest -q")
```

Acquire many:

```python
executions = pool.acquire(count=256)
```

The pool decides internally whether to:

```text
cold start
warm claim
local fork
reset / recycle
```

The user requests semantics and capacity; the system chooses mechanism.

### 4.3 Framework Integration

Most RL frameworks expose an environment abstraction rather than a generic storage backend.

The integration path should therefore be:

```text
RL / Eval Framework
        |
Environment / EnvironmentFactory
        |
Ramet Adapter
        |
ExecutionPool
```

A generic adapter can follow a `reset / step / close` model.

```python
class RametEnv:
    def __init__(self, pool):
        self.pool = pool
        self.execution = None

    def reset(self):
        if self.execution is None:
            self.execution = self.pool.acquire()
        else:
            self.pool.reset(self.execution)
        return self._observation()

    def step(self, action):
        return self._apply_action(action)

    def close(self):
        if self.execution is not None:
            self.pool.release(self.execution)
```

Framework-specific adapters should remain thin.

### 4.4 Protocol-First Integration

Initially, two protocol-level integrations are enough.

#### Workspace Protocol

For production:

```text
mount
checkpoint
resume
fork
```

#### Environment Protocol

For parallel / RL / eval:

```text
reset
step
state
close
```

This keeps framework-specific integrations small.

### 4.5 Declarative Policy

Normal users should configure desired behavior:

```python
pool = store.execution_pool(
    profile="parallel",
    concurrency=1000,
)
```

not low-level mechanisms:

```text
overlay implementation
native branch mode
reflink reset
warm-pool internals
```

Expert escape hatches can exist, but automatic strategy selection should be the default.

### 4.6 SDK Strategy

Client SDK priorities:

```text
Python      first-class
Go          first-class
TypeScript  useful for application/control-plane integration
```

SDKs should expose user concepts and should not mechanically mirror internal Go interfaces.

A useful DX test is:

> If an RL researcher needs to understand `BranchableStateStore`, the API layering has failed.

---

## 5. Core Design Principles

### 5.1 Localize the Hot Path

Remote metadata stores should not serve every `stat`, `open`, `readdir`, `rename`, or temporary write.

Execution-local operations should remain local whenever possible.

### 5.2 Share Immutable State Aggressively

A thousand executions starting from the same image and repository should share those bytes.

The physical model should approach:

```text
one immutable base
+
N small mutable deltas
```

not:

```text
N full copies
```

### 5.3 Persist Selectively

> Persist irreproducible state, reference immutable state, reconstruct reproducible state, and discard ephemeral state.

### 5.4 Native Branching Is Optional

Native database branching is an acceleration path, not part of the correctness model.

A generic provider must remain correct without it.

### 5.5 Correctness Is Environment-Level

A snapshot is not correct merely because file bytes match.

Restore must reproduce the observable state required by the workload.

### 5.6 Reset Is as Important as Fork

High-throughput workloads may prefer:

```text
create once
run
reset
run
reset
run
```

over repeatedly creating new durable branches or full sandboxes.

### 5.7 Single Writer First

The initial system should optimize for one active writer per execution.

General distributed multi-writer semantics are not a V1 goal.

### 5.8 Operational Simplicity

Self-hosting should be easy.

A user should be able to start locally with one command and grow toward a distributed deployment without changing the core state model.

Kubernetes and additional distributed dependencies are optional scaling tools, not baseline requirements.

---

## 6. Core State Model

The core concepts are:

- **Environment**
- **Environment Manifest**
- **Snapshot**
- **Durable Branch**
- **Execution**
- **Checkpoint**
- **Promotion**
- **External Binding**

### 6.1 Environment

An environment is a logical state container.

It may contain:

```text
filesystem
structured state
runtime image
Git base
artifacts
logical time
identity policy
external bindings
```

An environment does not imply a running sandbox.

### 6.2 Environment Manifest

A manifest is an immutable description of an environment snapshot.

It stores references rather than duplicating all bytes.

```yaml
environment_id: env-123
snapshot_id: snap-42

runtime:
  image: sha256:img-abc

workspace:
  git:
    remote: https://example.com/org/repo
    commit: 83ac02
  durable_snapshot: fs-17

structured_state:
  snapshot: app-state-9

toolchain:
  digest: sha256:toolchain-def

clock:
  mode: virtual
  epoch: 2026-01-01T09:00:00Z

identity:
  regenerate_on_fork: true

external_bindings:
  - name: source-control
    mode: proxied
    credential_ref: secret://agent-token
```

The manifest is the root of reproducibility.

### 6.3 Snapshot

A snapshot is an immutable durable state reference.

It may be used to:

- resume,
- fork,
- reset,
- reproduce an evaluation,
- create a durable branch.

### 6.4 Durable Branch

A durable branch is an independently mutable, long-lived lineage.

A backend may implement it using:

- native database branching,
- versioned rows,
- persistent trees,
- copy-on-write manifests,
- another snapshot-capable representation.

### 6.5 Execution

An execution is a running or runnable local state derived from a snapshot.

```text
Snapshot S0
   |
Execution E1
   |
local mutable delta
```

### 6.6 Checkpoint

A checkpoint captures a consistent state boundary.

Checkpoints may be:

```text
local
durable
```

Local checkpoints optimize short rollback loops.

Durable checkpoints support resume, sharing, audit, and long-running production state.

### 6.7 Promotion

Promotion converts selected execution-local state into a durable snapshot or branch.

### 6.8 External Binding

An external binding describes state outside Ramet's direct rewind boundary.

Examples:

- third-party APIs,
- credential providers,
- payment systems,
- remote SaaS,
- external queues.

These bindings must be explicit because local rollback cannot necessarily undo their side effects.

---

## 7. High-Level Architecture

```text
                           Control Plane
                                 |
                   Auth / Tenant / Policy
                                 |
                    Workspace / ExecutionPool
                                 |
                        Lifecycle Service
                                 |
                 Snapshot / Restore / Verify
                                 |
             +-------------------+-------------------+
             |                                       |
       Durable State                             Content
             |                                       |
        StateStore                               ContentStore
             |                                       |
   manifests / lineage / refs                immutable objects

================================================================

                            Data Plane
                                 |
                          Worker / Sandbox
                                 |
                      Shared Content Cache
                                 |
                         ExecutionStore
                                 |
                 +---------------+---------------+
                 |               |               |
             Execution A     Execution B     Execution C
              COW delta        COW delta        COW delta
```

The durable control plane is deliberately kept off the high-frequency filesystem syscall path.

---

## 8. State Components and Snapshot Coordination

An environment may consist of multiple independently managed state components.

Possible components:

```text
FilesystemComponent
StructuredStateComponent
ClockComponent
RuntimeComponent
TerminalComponent
IdentityComponent
ExternalBindingComponent
```

A component interface may look like:

```go
type StateComponent interface {
    PrepareSnapshot(ctx context.Context) error

    Snapshot(
        ctx context.Context,
    ) (ComponentSnapshot, error)

    Restore(
        ctx context.Context,
        snapshot ComponentSnapshot,
    ) error

    Verify(
        ctx context.Context,
        snapshot ComponentSnapshot,
    ) (VerificationResult, error)
}
```

Components needing post-fork rebinding may implement:

```go
type ForkAwareComponent interface {
    RebindAfterFork(
        ctx context.Context,
        fork ForkContext,
    ) error
}
```

### 8.1 Snapshot Classes

V1 primarily needs **workspace snapshots**.

```go
type SnapshotClass string

const (
    SnapshotWorkspace SnapshotClass = "workspace"
    SnapshotExecution SnapshotClass = "execution"
)
```

A workspace snapshot contains logical durable state:

- filesystem namespace,
- file contents,
- structured state,
- manifest,
- reconstruction recipes,
- logical time metadata.

An execution snapshot may additionally contain:

- process memory,
- open file descriptors,
- shell / PTY state,
- in-memory runtime state.

Execution snapshots are more provider-specific and are not required for V1.

### 8.2 Cross-Component Consistency

A snapshot composed of arbitrary component points can be invalid:

```text
filesystem @ T1
database   @ T2
clock      @ T3
```

Ramet therefore needs a snapshot coordinator.

Conceptually:

```text
1. Request snapshot barrier
2. Quiesce participating components
3. PrepareSnapshot()
4. Capture component snapshots
5. Build immutable manifest
6. Atomically publish manifest
7. Resume components
```

The coordinator may expose consistency levels:

```text
best_effort
workspace_consistent
environment_consistent
execution_consistent
```

Not every workload requires the strongest level.

---

## 9. Restore Fidelity, Identity, and Time

Restore correctness is defined by observable behavior, not only byte equality.

At minimum, verification may check:

```text
expected Git revision
expected dirty-file set
expected workspace manifest
expected structured-state snapshot
expected logical time
expected runtime image
no unexpected process
no unexpected writable mount
```

The restore path should be:

```text
restore
   |
rebind
   |
verify
   |
ready
```

### 9.1 Identity and Randomness

Forking can duplicate state that should be unique:

- machine identity,
- cached UUID state,
- RNG state,
- leases,
- session identifiers,
- short-lived credentials.

The manifest should define policy explicitly:

```yaml
identity:
  regenerate_on_fork: true

randomness:
  seed_policy: per_execution

clock:
  mode: virtual
  epoch: 2026-01-01T09:00:00Z
```

For reproducible experiments, deterministic seed policies may be selected intentionally.

### 9.2 Quarantine

A reset execution that fails verification should not return to the pool.

```text
reset
   |
rebind
   |
verify
   |
+-- valid ------> ready
|
+-- invalid ----> quarantine -> diagnose -> destroy/rebuild
```

Useful metrics:

```text
reset_success_rate
verification_failure_rate
quarantine_rate
reuse_count
```

Silent contamination is more dangerous than an explicit failure because it can corrupt evaluation or reward results.

---

## 10. Shared Immutable Content

Many environment inputs are identical across executions.

Typical shared content:

- OCI/container image layers,
- OS layers,
- language runtimes,
- compilers,
- toolchains,
- Git repository bases,
- remote Git objects,
- model weights,
- datasets,
- dependency bundles.

A worker should maintain a shared content-addressed cache.

```text
                         Worker Node
                              |
                   Shared Content Cache
                   ====================
                   image layers
                   Git repository base
                   toolchains
                   dependencies
                   models
                   datasets
                              |
             +----------------+----------------+
             |                |                |
         Execution A      Execution B      Execution C
         writable         writable         writable
          delta            delta            delta
```

### 10.1 Content Store

```go
type ContentStore interface {
    Get(
        ctx context.Context,
        ref ContentRef,
    ) (io.ReadCloser, error)

    RangeGet(
        ctx context.Context,
        ref ContentRef,
        offset int64,
        length int64,
    ) (io.ReadCloser, error)

    Put(
        ctx context.Context,
        ref ContentRef,
        r io.Reader,
    ) error

    Prefetch(
        ctx context.Context,
        refs []ContentRef,
    ) error

    Pin(
        ctx context.Context,
        ref ContentRef,
    ) error

    Unpin(
        ctx context.Context,
        ref ContentRef,
    ) error
}
```

### 10.2 Lazy Hydration

Large environments should not necessarily be fully downloaded before they become runnable.

```text
manifest ready
     |
mount metadata
     |
execution starts
     |
content hydrated on demand
```

Known hot content may be prefetched:

```text
entrypoint
agent binary
Git index
top-level source
frequently used toolchain files
```

---

## 11. Semantic Persistence

Ramet divides state into four classes.

| Class | Meaning | Examples | Default Action |
|---|---|---|---|
| Shared Immutable | Safely shared, immutable input | image layers, Git base, model | reference/cache |
| Durable | Cannot safely be reconstructed | source changes, local commits | persist |
| Rebuildable | Reconstructable from durable inputs | dependencies, build cache | cache + recipe |
| Ephemeral | Should not survive execution | sockets, locks, temp files | discard |

A policy model:

```go
type PersistenceClass int

const (
    SharedImmutable PersistenceClass = iota
    Durable
    Rebuildable
    Ephemeral
)
```

Example:

```yaml
paths:
  - pattern: "/workspace/src/**"
    class: durable

  - pattern: "/workspace/target/**"
    class: rebuildable

  - pattern: "/tmp/**"
    class: ephemeral
```

A durable checkpoint behaves differently by class:

```text
SharedImmutable -> record reference
Durable         -> persist changed state
Rebuildable     -> record provenance / cache key
Ephemeral       -> ignore
```

---

## 12. Git-Aware Workspace Profile

Git-aware optimization is the first specialized profile.

A repository should not be treated as an opaque directory tree.

### 12.1 Repository Base

A fixed remote commit is shared immutable state:

```text
                         repo@C123
                            |
             +--------------+--------------+
             |              |              |
         Execution A    Execution B    Execution C
          overlay        overlay        overlay
```

### 12.2 Git Object Provenance

Persistence depends on provenance.

```text
remote:
A -- B -- C

local:
A -- B -- C -- D
```

Objects reachable from the remote state are reconstructable.

Objects reachable only from local commit `D` are durable until externalized.

Therefore:

> Git persistence is provenance-based, not merely path-based.

### 12.3 Workspace Model

A coding workspace can be modeled as:

```text
Workspace =
    Git Base
    +
    Durable Working-Tree Delta
    +
    Durable Local Git State
    +
    Rebuildable Dependencies / Build Cache
    +
    Ephemeral Runtime State
```

Example:

```text
base:
    repo@C123

delta:
    MODIFY src/foo.go -> blob X
    ADD    src/new.go -> blob Y
    DELETE src/old.go

local_git:
    branch: agent/attempt-1
    commits: [D, E]

rebuildable:
    node_modules -> recipe(lockfile hash, runtime version)
```

Common rebuildable paths include:

```text
node_modules/
target/
bazel-out/
.gradle/
.m2/
Go build cache
package caches
compiler caches
```

Production may retain these in cache for resume latency.

Parallel workloads may discard them aggressively unless reuse probability is high.

---

## 13. Durable State and Provider SPI

The durable layer stores:

- manifests,
- snapshot lineage,
- namespace metadata,
- file-to-content references,
- structured state,
- durable checkpoint metadata,
- policies.

### 13.1 Base `StateStore`

```go
type StateStore interface {
    Create(
        ctx context.Context,
        req CreateRequest,
    ) (StateRef, error)

    Snapshot(
        ctx context.Context,
        state StateRef,
    ) (SnapshotRef, error)

    Open(
        ctx context.Context,
        ref StateRef,
    ) (StateHandle, error)

    Delete(
        ctx context.Context,
        ref StateRef,
    ) error
}
```

Every provider must support the correctness baseline without native branching.

### 13.2 Optional Native Branching

```go
type BranchableStateStore interface {
    StateStore

    Fork(
        ctx context.Context,
        parent SnapshotRef,
        opts ForkOptions,
    ) (StateRef, error)
}
```

Native branching is an acceleration path.

### 13.3 Capability Discovery

```go
type CapabilityProvider interface {
    Capabilities(
        ctx context.Context,
    ) (Capabilities, error)
}

type Capabilities struct {
    NativeBranching   bool
    PointInTimeFork   bool
    NestedBranching   bool
    AtomicPromotion   bool
    BranchTTL         bool
    SuspendResume     bool
}
```

Provider decisions should depend on semantic capabilities, not provider-name checks.

### 13.4 Durable Fork Strategy

```go
type ForkStrategy string

const (
    ForkStrategyAuto     ForkStrategy = "auto"
    ForkStrategyNative   ForkStrategy = "native"
    ForkStrategyEmulated ForkStrategy = "emulated"
)
```

Default:

```text
auto:
    native provider fork if supported
    otherwise generic versioned-state fork
```

`native` is an expert override.

`emulated` is useful for portability, debugging, and benchmark comparison.

### 13.5 Lineage Compaction

Generic branch lineage must not create unbounded read amplification.

```text
S0 -> S1 -> S2 -> ... -> S100
```

The system therefore needs:

```text
flatten
squash
materialize
rebase snapshot
```

Compaction policy may consider:

- lineage depth,
- read amplification,
- branch lifetime,
- snapshot popularity,
- write density,
- storage cost.

---

## 14. Local Execution Engine

The execution layer serves the hot path.

Common operations include:

```text
stat
open
readdir
rename
find
grep
compiler scans
package-manager scans
Git scans
```

These should normally be served locally.

A worker may maintain:

```text
Local Runtime
 |
 +-- namespace cache
 +-- inode / metadata cache
 +-- content cache
 +-- local writable overlay
 +-- dirty-state tracker
 +-- local journal
```

Possible implementations:

- OverlayFS,
- reflink-capable filesystems,
- local snapshot engines,
- embedded metadata stores,
- local filesystem plus journal.

### 14.1 `ExecutionStore`

```go
type ExecutionStore interface {
    Materialize(
        ctx context.Context,
        snapshot SnapshotRef,
    ) (ExecutionRef, error)

    Fork(
        ctx context.Context,
        parent ExecutionRef,
    ) (ExecutionRef, error)

    Reset(
        ctx context.Context,
        execution ExecutionRef,
        snapshot SnapshotRef,
    ) error

    Checkpoint(
        ctx context.Context,
        execution ExecutionRef,
    ) (LocalCheckpointRef, error)

    Discard(
        ctx context.Context,
        execution ExecutionRef,
    ) error
}
```

For high-fan-out workloads, this is the primary branching path.

One rollout should not imply one remote durable branch by default.

---

## 15. Execution Lifecycle and Strategy Selection

Core lifecycle operations:

```text
Create
Materialize
Fork
Reset
Recycle
Checkpoint
Verify
Discard
Promote
Resume
```

### 15.1 Fork

Creates an isolated execution from a snapshot or existing execution base.

### 15.2 Reset

Returns an existing execution to a known snapshot.

### 15.3 Recycle

Recycle is:

```text
stop workload
   |
reset
   |
rebind identity / credentials / network
   |
verify
   |
return to ready pool
```

### 15.4 Discard

Deletes execution-local state without creating durable state.

### 15.5 Promote

Persists selected local state and publishes a durable snapshot.

### 15.6 Execution Strategies

```go
type ExecutionStrategy string

const (
    StrategyColdStart ExecutionStrategy = "cold_start"
    StrategyWarmClaim ExecutionStrategy = "warm_claim"
    StrategyFork      ExecutionStrategy = "fork"
    StrategyRecycle   ExecutionStrategy = "recycle"
)
```

The system may choose based on:

- requested SLO profile,
- content locality,
- warm-pool availability,
- reset fidelity history,
- expected execution duration,
- fan-out,
- worker pressure,
- snapshot compatibility.

Automatic selection should be the default.

---

## 16. Production and Parallel Execution Policies

### 16.1 Production

Production is **durable by default**.

```text
write
  |
local mutation
  |
writeback / checkpoint
  |
upload immutable content
  |
commit durable metadata
```

Strong durability ordering:

```text
1. Finalize local content
2. Compute digest
3. Upload immutable content
4. Confirm content durability
5. Commit metadata referencing content
6. Return durable success
```

This prevents durable metadata from referencing missing content.

A crash after content upload but before metadata commit creates an orphan object, which is safe to garbage-collect later.

### 16.2 Parallel / RL / Eval

Parallel execution is **ephemeral by default**.

```text
write
  |
local mutation
  |
evaluate
  |
discard OR promote
```

For a discarded execution:

```text
remote state writes ~= 0
```

For promotion:

```text
freeze local delta
   |
identify durable changes
   |
upload missing content
   |
persist metadata/state
   |
publish manifest
```

The goal is maximum state sharing and minimum per-execution durable amplification.

---

## 17. Reset, Recycle, and Warm Pools

Reset and reuse are first-class throughput optimizations.

A warm-pool lifecycle:

```text
pre-create execution
       |
     ready
       |
     claim
       |
      run
       |
     reset
       |
     rebind
       |
     verify
       |
  +----+----+
  |         |
 ready   quarantine
```

Benefits:

- lower control-plane churn,
- less sandbox provisioning,
- better cache reuse,
- lower startup cost.

Risks:

- hidden process leakage,
- stale writable state,
- stale credentials,
- stale logical time,
- resource leakage,
- result contamination.

Reset must therefore be paired with verification.

---

## 18. External Side Effects and Secrets

### 18.1 External Side Effects

Not all state is rewindable.

Examples:

```text
send email
POST webhook
charge payment
create remote issue
write external database
publish message
```

A local rollback cannot undo these actions.

The environment must distinguish:

```text
managed rewindable state
external non-rewindable state
```

Possible policies:

- proxy or simulator,
- record/replay,
- idempotent external calls,
- compensating action,
- explicitly unmanaged external effect.

The manifest should expose rewindability rather than pretending every environment is fully reversible.

### 18.2 Secrets and Credentials

Long-lived secrets should not be snapshotted as ordinary bytes.

Prefer:

```text
snapshot:
    SecretRef("source-control-token")
```

Restore:

```text
SecretRef
    |
credential broker
    |
short-lived scoped credential
```

Benefits:

- credentials are not duplicated across forks,
- secrets can rotate independently,
- permissions can be scoped per execution,
- sandbox processes do not need broad storage credentials.

---

## 19. Scalability, Scheduling, and Multi-Tenancy

### 19.1 Metadata Scalability

Suppose:

```text
10,000 executions
x 100 metadata operations / second
= 1,000,000 metadata operations / second
```

The durable state store should not need to serve this traffic.

Split metadata into:

#### Hot Execution Metadata

```text
stat
lookup
open
readdir
rename
temporary create
```

Handled locally.

#### Durable Lifecycle Metadata

```text
create environment
publish snapshot
create durable branch
promote
delete
update lineage
GC metadata
```

Handled by the control plane.

The scaling goal is:

> Remote metadata traffic scales with lifecycle events, not filesystem syscall volume.

### 19.2 Content-Aware Scheduling

Placement should consider cached state.

A worker may already have:

```text
image X
repository Y
toolchain Z
model M
```

A scheduler can consider:

```text
score(node) =
    resource_fit
    + content_locality
    + snapshot_compatibility
    + tenant_fairness
```

This matters most for large images, repositories, datasets, models, and short-lived workloads.

### 19.3 Multi-Tenancy

Logical hierarchy:

```text
Tenant
 |
 +-- Environment / Workspace
       |
       +-- Snapshot
       +-- Durable Branch
       +-- Executions
```

Persistent identifiers must be tenant scoped.

Useful limits include:

```text
storage quota
concurrent execution quota
fork/reset rate
promotion rate
content bandwidth
durable branch quota
cache policy
```

One tenant should not be able to exhaust shared control-plane capacity with a burst.

### 19.4 Content Isolation

Initial storage should favor tenant isolation.

Example:

```text
tenant/<tenant-id>/content/<digest>
```

Cross-tenant deduplication should not be enabled by default because it complicates:

- deletion,
- encryption,
- side-channel resistance,
- accounting,
- residency.

---

## 20. Failure Handling, GC, and Observability

### 20.1 Failure Model

#### Worker Crash

Production:

```text
recover from durable state
optionally replay a durable local journal
```

Parallel execution:

```text
lose isolated execution
retry or discard
```

#### Content Upload Succeeds, Metadata Commit Fails

Result:

```text
orphan immutable object
```

Recovered by GC.

#### Metadata References Missing Content

Prevent by durability ordering.

#### Promotion Interrupted

Promotion must be:

```text
idempotent
retryable
observable
```

#### Reset Verification Fails

```text
quarantine execution
do not reuse
```

#### Provider Failure

Do not publish a partially created logical snapshot or branch.

### 20.2 Garbage Collection

A safe design:

```text
mark durable manifests
      |
walk reachable content refs
      |
protect in-flight promotion refs
      |
apply grace period
      |
sweep unreachable content
```

GC must understand:

- snapshots,
- durable branches,
- promotion transactions,
- pinned content,
- tenant boundaries.

### 20.3 Observability

Production metrics:

```text
mount latency
resume latency
p50/p95/p99 filesystem latency
checkpoint latency
durability lag
recovery latency
cache hit rate
bytes hydrated
bytes uploaded
```

Parallel metrics:

```text
executions/sec
successful executions/sec/$
fork latency
reset latency
recycle latency
time-to-ready
straggler distribution
remote writes/execution
local storage amplification
cache hit rate
verification failure rate
quarantine rate
```

Long-running stability metrics:

```text
throughput drift
memory growth
mount count growth
orphan execution count
GC backlog
cache churn
disk pressure
cleanup failures
retry storm rate
```

---

## 21. Deployment and Operations

Ramet should be easy to self-host.

Operational simplicity is a product requirement.

Users should be able to move through:

```text
local laptop
    |
single VM
    |
small production deployment
    |
distributed deployment
```

without changing the core state model.

### 21.1 Deployment Tiers

#### Local Development

Goal:

> Clone the repository and run a useful system in minutes.

Possible experience:

```bash
ramet dev
```

or:

```bash
docker compose up
```

A local setup may use:

```text
Ramet server     one process
Ramet worker     local process
Metadata               embedded DB or local Postgres
Content                local filesystem or MinIO
Cache                  local disk
Auth                   development token
```

No cloud account should be required.

#### Single-Node Self-Hosted

A first production shape:

```text
+--------------------------------------+
|             One VM / Host            |
|                                      |
|  ramet server + worker          |
|  local cache                         |
|                                      |
+------------------+-------------------+
                   |
          +--------+--------+
          |                 |
       Postgres          Object Store
                       S3 / GCS / MinIO
```

A small team should not need a cluster.

#### Distributed

```text
                     Load Balancer
                           |
                 Ramet Control Plane
                     /      |       \
                    /       |        \
              Worker A   Worker B   Worker C
                  |          |          |
              local SSD  local SSD  local SSD
                    \        |        /
                     Durable State Services
```

Kubernetes may orchestrate this topology but is not a correctness dependency.

### 21.2 Minimal Required Dependencies

Baseline:

```text
Required:
    Ramet
    metadata/state backend
    object/content store

Optional:
    external cache
    message queue
    distributed scheduler
    Kubernetes
    tracing backend
```

Redis, Kafka, etcd, or similar systems should not become mandatory without a demonstrated need.

### 21.3 Few Binaries

Recommended:

```text
ramet-server
ramet-worker
ramet
```

Simple deployment:

```bash
ramet serve --with-worker
```

Larger deployment:

```bash
ramet server
ramet worker
```

### 21.4 Configuration

A basic production configuration should be short:

```yaml
server:
  listen: ":8080"

metadata:
  provider: postgres
  dsn: "${DATABASE_URL}"

content:
  provider: s3
  bucket: ramet-data
  region: us-west-2

worker:
  cache_dir: /var/lib/ramet/cache
  cache_size: 500GB
```

Users should not need to configure native branching, compaction mode, or overlay internals for a normal install.

### 21.5 Backend Portability

Baseline targets:

```text
Metadata / State:
    PostgreSQL
    local development backend
    optional snapshot-capable providers

Content:
    S3-compatible storage
    GCS
    MinIO
    local filesystem
```

### 21.6 Upgrade and Schema Safety

Requirements:

- versioned migrations,
- clear destructive-migration policy,
- documented server/worker version skew,
- manifest and snapshot format versions,
- rolling-upgrade guidance.

A snapshot should not become unreadable after a routine binary upgrade.

### 21.7 Backup and Recovery

Durable state separates naturally into:

```text
metadata / manifests
immutable content
```

Metadata can use ordinary database backup/PITR.

Immutable content can rely on object-store durability, replication, versioning, and lifecycle policy.

Operational commands should include:

```bash
ramet doctor
ramet verify
```

Checks include:

```text
metadata connectivity
content-store connectivity
referenced content exists
schema version
manifest readability
snapshot lineage consistency
worker health
disk pressure
provider capabilities
```

### 21.8 GC and Cache Operations

Safe GC defaults:

```text
mark
grace period
sweep
```

Useful commands:

```bash
ramet gc status
ramet gc dry-run
ramet gc run
```

Worker cache should be self-managing.

Operators configure high-level limits:

```yaml
worker:
  cache_size: 500GB
  min_free_disk: 50GB
```

The runtime manages:

- eviction,
- pinning,
- prefetch,
- corruption detection,
- disk pressure.

### 21.9 Worker Lifecycle

Workers should be disposable.

Graceful drain:

```bash
ramet worker drain
```

During drain:

- no new executions are assigned,
- production state is flushed as required,
- ephemeral work finishes or is retried,
- local reusable executions are discarded,
- expired leases are cleaned automatically.

### 21.10 Health and Observability Defaults

Expose:

```text
/live
/ready
/metrics
```

Baseline telemetry:

```text
Prometheus-compatible metrics
structured logs
OpenTelemetry traces where useful
```

Logs should carry stable IDs such as:

```text
tenant_id
workspace_id
snapshot_id
execution_id
worker_id
operation_id
```

but should not log file content or credentials.

### 21.11 Dependency Failure Behavior

If metadata is unavailable:

```text
existing local work may continue temporarily
durable checkpoint may block/fail
never falsely acknowledge durability
```

If object storage is unavailable:

```text
cached reads may continue
new durable writes cannot be acknowledged
```

If a worker loses control-plane connectivity:

```text
allow bounded autonomous behavior
do not indefinitely create untracked durable state
```

### 21.12 Authentication and Bootstrap

A local/self-hosted bootstrap should not require an external identity provider.

Possible progression:

```text
development token
    |
service tokens
    |
OIDC / short-lived scoped credentials
```

### 21.13 Operational Success Metric

A useful OSS target:

```text
local demo:
    one command

small production:
    Ramet
    + one SQL backend
    + one object store

upgrade:
    documented command sequence

backup:
    standard DB backup + object-store durability

worker replacement:
    no manual metadata surgery
```

If the first durable workspace requires operating multiple queues, caches, coordinators, and schedulers, the design has become too operationally complex.

---

## 22. Implementation Language

The default implementation language is **Go**.

Go fits the first version because the primary challenge is distributed-systems architecture, state semantics, cloud integration, and operational simplicity.

Advantages:

- simple concurrency primitives,
- mature networking libraries,
- strong cloud SDK support,
- mature SQL ecosystem,
- static binaries,
- fast OSS iteration,
- good profiling and operational tooling.

Suggested layout:

```text
cmd/
    ramet-server/
    ramet-worker/
    ramet-cli/

internal/
    controlplane/
    workspace/
    executionpool/
    lifecycle/
    snapshot/
    manifest/
    runtime/
    execution/
    content/
    statestore/
    cache/
    gitprofile/
    verification/
    promotion/
    gc/
    tenant/
```

### 22.1 Rust

Rust may offer real advantages for:

- FUSE hot paths,
- a high-performance local cache,
- zero-copy I/O,
- a memory-sensitive metadata engine,
- low-level Linux integration,
- tail-latency-sensitive data-plane components.

Those benefits should be demonstrated by benchmarks.

Recommended policy:

> Build V1 in Go, measure, and introduce Rust only for a demonstrated hot path.

A future split could be:

```text
Control plane         Go
Worker orchestration  Go
Hot filesystem path   Rust
```

---

## 23. Benchmark Plan

Benchmarks are part of the product.

The headline V1 benchmark should be a **parallel Git-aware environment benchmark**, not merely a workspace-resume benchmark.

### 23.1 Git-Aware Workspace Benchmark

Use realistic repositories with:

```text
large Git history / object set
large dependency tree
small source-code mutation
local commit
```

Measure:

```text
initial materialization
resume latency
checkpoint latency
fork latency
bytes persisted
bytes hydrated
cache hit rate
```

Compare:

```text
full workspace copy
ordinary persistent volume
generic filesystem snapshot
Git-aware Ramet
```

Main hypothesis:

> Persisting only irreproducible workspace state should drastically reduce checkpoint size and resume/fork work.

### 23.2 Fan-Out Benchmark

From one base snapshot:

```text
1
100
1,000
10,000 executions
```

Measure:

```text
time-to-ready
aggregate throughput
remote metadata operations
remote bytes
local disk amplification
```

### 23.3 Reset / Recycle Benchmark

Compare:

```text
cold create
warm claim
local fork
reset/recycle
```

Measure:

```text
executions/sec
control-plane operations
verification failures
resource leakage
```

### 23.4 Soak Test

Run continuously for hours.

Measure:

```text
throughput drift
memory growth
disk growth
cache-hit drift
GC backlog
cleanup failures
quarantine rate
```

Strong short benchmarks are insufficient without stable long-running throughput.

### 23.5 Production Reliability Benchmark

Measure:

```text
worker crash recovery
checkpoint correctness
resume p99
durability lag
cold-cache resume
warm-cache resume
```

---

## 24. V1 Scope

V1 should prove the parallel-agent wedge, not the entire long-term state-plane vision.

### 24.1 User-Facing Experience

Build the first parallel-execution experience around `ExecutionPool`.

Primary DX:

```python
pool = store.execution_pool(
    base=base,
    profile="parallel",
    concurrency=1000,
)

executions = pool.acquire(count=1000)
```

The user should be able to:

```text
prepare one base environment
        |
launch many isolated executions
        |
run agents
        |
reset / recycle / discard
        |
optionally promote selected state
```

The first benchmark workload should use Git-aware coding environments.

A secondary `Workspace` API may exist to exercise durability primitives, but it is not the primary V1 product story.


### 24.2 Core System

Build:

- immutable Git repository base,
- shared dependency / toolchain references,
- worker-local shared content cache,
- local writable COW execution state,
- `ExecutionPool`,
- local fork,
- reset,
- recycle,
- verification and quarantine,
- discard,
- semantic persistence classes,
- content-addressed content store,
- generic versioned state store,
- optional durable checkpoint,
- basic promotion,
- benchmark and soak-test suite.

The V1 correctness contract should prioritize:

```text
clean reset
execution isolation
shared-base immutability
no cross-execution state leakage
```

The V1 performance contract should prioritize:

```text
time-to-ready
executions/sec
reset/recycle throughput
remote bytes per execution
local storage amplification
long-running stability
```


### 24.3 Operations

Build:

- one-command local development,
- Docker Compose example,
- single-node self-hosted deployment,
- short YAML configuration,
- PostgreSQL + S3-compatible reference deployment,
- local filesystem / MinIO development backend,
- `/live`, `/ready`, `/metrics`,
- `ramet doctor`,
- versioned schema migrations,
- worker drain and cleanup.

### 24.4 Optional Provider Optimization

A native durable-branching adapter may be implemented if convenient.

It must not be required for correctness.

### 24.5 Explicitly Deferred

Do not build yet:

- general process-memory snapshot,
- full terminal/PTY snapshot,
- general external-side-effect rollback,
- arbitrary multi-writer coherence,
- semantic merge engine,
- global scheduler,
- full RL framework,
- global cross-tenant deduplication.

---

## 25. Development Roadmap

### Phase 0 — Local Parallel-State Prototype

Build:

```text
immutable Git base
shared local content cache
local COW execution
fork
reset
verify
discard
semantic persistence classifier
```

Goal:

Prove that many isolated executions can share one base while keeping mutable state independent.


### Phase 1 — Parallel Git-Aware Execution

Add:

```text
ExecutionPool
Python SDK
environment adapter
content-addressed remote store
versioned durable metadata
large fan-out
reset/recycle
verification/quarantine
basic promotion
```

Operational baseline:

```text
single-node deployment
Docker Compose
health checks
doctor command
schema migration
worker drain
```

Goal:

Run hundreds to thousands of isolated coding-agent executions from one shared base with stable throughput.


### Phase 2 — Scale and Stability

Add:

```text
warm pools
content-aware placement signals
lazy hydration
prefetch
cache pressure management
multi-tenant namespace
resource isolation
soak testing
GC hardening
observability
```

Goal:

Demonstrate stable high-throughput operation over long-running parallel workloads.


### Phase 3 — Durable Production Workspace

Add:

```text
Workspace API
durable writeback
journal
failure recovery
checkpoint / resume
low-latency mount
production SLOs
```

Goal:

Reuse the same state model for persistent production-agent workspaces.


### Phase 4 — Structured State Components

Add:

```text
structured application state
snapshot coordinator
logical clock
external bindings
```

Goal:

Move beyond filesystem-centric environments.

### Phase 5 — Advanced Execution State

Explore only when justified:

```text
process memory
terminal/session state
provider-specific execution snapshots
```

---

## 26. Open Questions

### Local Development Backend

Should local development use an embedded SQL engine, automatically managed local Postgres, or Docker Compose by default?

### Workspace Mount Contract

Should production platforms integrate primarily through FUSE, a sandbox-side daemon, or a runtime SDK?

### Git Materialization

Can the shared Git base be exposed without fully materializing an independent working tree per execution?

### Content Granularity

When should whole-file content evolve into chunked storage?

### Snapshot Consistency

How much coordination is required between filesystem, structured state, logical time, and runtime components?

### Verification Cost

Can verification be tiered?

```text
cheap on every reset
deep every N resets
```

### Reset vs Fork

When should the system recycle an execution versus create a fresh local fork?

### Reuse Limits

Should executions have a maximum reuse count even when verification succeeds?

### Structured State Local Fork

How should database-like application state be cheaply forked in the local execution layer?

### External Effects

What API best communicates rewindability and side-effect guarantees?

### Provider Capability Evolution

How should capability semantics evolve without leaking provider-specific behavior into the public API?

### Compaction

What metrics should trigger lineage flattening?

### Execution Snapshots

Can workspace snapshots plus warm pools deliver enough value that process-memory snapshots remain unnecessary?

### ExecutionPool Policy

Which controls belong in the public API, and which should remain automatic?

### Upgrade Compatibility

What server/worker version skew and snapshot-format compatibility guarantees should be supported?

---

## 27. Success Criteria

### 27.1 V1

Ramet can:

```text
prepare one Git-aware coding environment
share its immutable base
launch hundreds or thousands of isolated executions
keep per-execution mutable state small
reset executions cleanly
detect reset contamination
discard state cheaply
run for hours without throughput collapse
```

The system is also easy to self-host.


### 27.2 Production

Ramet provides:

```text
predictable resume latency
bounded durability lag
crash recovery
high cache efficiency
simple operations
```

### 27.3 Parallel / RL / Eval

The same state model can later provide:

```text
high fan-out
cheap reset/recycle
stable sustained throughput
low remote write amplification
strong reset verification
```

### 27.4 Architectural

The same state semantics support production and parallel workloads without forcing them through the same execution path.

---

## 28. Final Thesis

Ramet is not fundamentally a filesystem and is not fundamentally an RL system.

It is a state infrastructure layer for agents.

The long-term model is:

```text
                         Agent State Plane
                                |
                 snapshot / fork / reset
                checkpoint / resume / promote
                                |
             +------------------+------------------+
             |                                     |
       Production Profile                    Parallel Profile
             |                                     |
       latency + durability                throughput + sharing
             |                                     |
             +------------------+------------------+
                                |
                     Common State Semantics
                                |
                +---------------+---------------+
                |                               |
        Durable State Layer             Local Execution Layer
                |                               |
       versioned metadata/state              local COW
       immutable content                     shared cache
```

The first open-source release should stay focused:

> **High-throughput state infrastructure for parallel agent environments.**

The first optimized state profile is:

> **Git-aware coding environments.**

The first release should prove one idea exceptionally well:

> **One shared environment base can safely and efficiently support a large population of isolated agent executions.**

The underlying storage principle remains:

> **Share what is immutable, isolate what changes, and persist only what must survive.**

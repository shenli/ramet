# Security policy

Ramet 0.3 is a trusted-worker alpha with credential-scoped tenant stores. It
manages filesystem state and API authorization; it does not provide a sandbox,
process isolation or a credential broker.

Mount **only the acquired execution directory** into your container or VM. Keep
the worker data directory, immutable objects, API token, and lease tokens outside
the workload's reach. A process running directly on the host as the worker's Unix user can bypass
filesystem permissions and corrupt its storage. Reflinks isolate writes; they
do not restrict which host files a process can open.

The single-worker API can capture directories readable by the worker unless
source restrictions are enabled. A bearer token administers its selected store.
The tenant gateway requires unique credentials, separate storage roots and
source/Git allowlists; tenant identity is never taken from a request header.
Allowlist configuration is trusted operator input. Do not grant broad host roots. The CLI
requires `RAMET_TOKEN` for non-loopback listeners. Use a private network and a
TLS reverse proxy for access across hosts. Single-worker health endpoints reveal only liveness and readiness state. The
tenant gateway exposes only `/live` publicly; readiness, metrics and APIs require
a tenant token. Tokenless mode is
for trusted local development only.

Never place secrets in a snapshot source. Path policy is explicit, defaults to
durable, and applies exclusions during promotion; initial snapshots and complete
checkpoints capture all supported files. Ramet does not discover secrets for you.
Secret references and credential rebinding remain roadmap features.

Stop all workload processes, unmount execution directories, and close writer
handles before reset, release, fork, or capture. New directory identities prevent
ordinary stale handles from writing the next tree; they cannot stop a privileged
process that finds and reopens the new path. `Execution.run()` is a same-host
convenience for trusted commands and kills ordinary POSIX process groups. It is
not containment for hostile processes that create new sessions or escape groups.

To report a vulnerability privately, use GitHub's **Report a vulnerability**
feature if enabled on this repository. Otherwise contact the maintainer through
the contact information on their GitHub profile and request a private channel.
Do not post exploit details, tokens, or private source data in a public issue.
Keep the Go toolchain at or above the patched minimum in `go.mod`.
There is no supported stable release or security-response SLA yet.

Execution leases default to 15 minutes. Expiry fences API calls and removes the
workspace; the runtime must stop its workload when ownership is lost. Automatic
Python renewal is a convenience, not process containment. The gateway enforces tenant authorization above independently owned PostgreSQL
namespaces. Database/bucket credentials are never exposed to workloads. A shared
gateway is not a hard process-resource boundary; use separate worker processes
for control-plane CPU/memory isolation. Use database and bucket
credentials restricted to this deployment; the reference Compose credentials
are supplied through environment variables.

The optional Linux FUSE backend verifies content before a file open succeeds.
It is an optimization inside the private worker boundary, not a sandbox. Mount
only the execution root into an isolated runtime; never mount its backing tree,
parent execution directory or worker data root. Stop writers and release runtime
mounts before reset, capture, teardown or recovery. The default copy backend
remains available without FUSE privileges.

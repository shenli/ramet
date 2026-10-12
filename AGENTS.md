# Working on Ramet

Ramet is a workspace-state engine. Start with `README.md`, `docs/architecture.md`,
and `docs/development.md`. For an agent consuming Ramet rather than modifying
it, use `docs/agents.md` and `sdk/python/README.md`.

- Public Go packages: `engine`, `client`, `storage`. HTTP implementation:
  `internal/server`. CLI: `cmd/ramet`. Python SDK: `sdk/python/ramet`.
- Run `make check` for changes to Go/Python behavior. Add focused regression
  coverage for persistence, lease, recovery, and request-validation changes.
- Linux FUSE and provider tests need their real prerequisites; skipped tests
  are not evidence those backends work. Follow `docs/fuse.md` and
  `docs/development.md` when changing those paths.
- Keep execution writes local until explicit capture/promotion. Preserve lease
  fencing and runtime cleanup before workspace reset/reuse.
- Never retry an uncertain mutation under a fresh operation ID. Receipt expiry
  or worker-session changes must fail closed, not silently repeat an effect.
- Keep private planning documents, credentials, raw sessions, caches, and test
  volumes untracked. Use disposable local resources for testing.
- Keep documentation claims aligned with executable evidence. Describe alpha
  limitations explicitly; do not turn test results into unmeasured scale claims.

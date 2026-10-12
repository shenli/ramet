# Public alpha launch checklist

Release scope: **0.3.0-alpha.1** (Python **0.3.0a1**), a trusted-worker state
engine. Copy/reflink is the default; Linux lazy hydration remains opt-in.
No production durability/availability SLO is claimed.

## Source and artifacts

- Apache-2.0 `LICENSE`, `NOTICE`, and linked dependency notices are included.
- Internal planning, raw testing sessions, caches, and provider volumes are
  excluded from the current Git tree and the Docker build context. Separately
  audit history: removal from the current tree does not erase old versions.
- Run a redacted secret scan over all Git refs and the candidate checkout.
  Review Actions logs/artifacts and commit metadata before changing visibility.
- Run `make check`, real PostgreSQL/S3/FUSE checks, the Docker lifecycle test,
  and the best-of-N example. Apply-check the exported winning patch.
- Follow the README from a clean checkout. Build and inspect archives and the
  wheel; test the installed wheel without importing the checkout's SDK.
- Require both **CI** and **Security** workflows to pass on the candidate commit.
  Follow [release.md](release.md) to generate downloadable candidates.

## Known limitations to retain at launch

- The earlier pressure timeout (F3) remains unexplained despite clean reruns.
  Preserve the [hardening report](testing/2026-10-09-hardening.md); investigate
  with captured goroutine stacks and request correlation if reproduced.
- Hostile tenant boundary coverage remains partial. Docker integration does
  not establish containment against container escapes or replace a supervisor.
- Caller death may leave runtime containers behind; reclaim them before their
  workspaces. Cleanup failure must never be ignored to force lifecycle progress.
- Git submodules/LFS need pre-hydration. Arbitrary-byte filenames, remote GC,
  distributed scheduling, live workspace durability, and memory snapshots are
  not part of this alpha.

## GitHub settings at visibility change

Dependabot alerts and automated security fixes are enabled. Actions default
permissions are read-only and cannot approve pull requests. Weekly dependency
updates and secret/vulnerability scans are configured in the repository.

GitHub currently refuses branch rules for this private repository on its plan
(HTTP 403); private vulnerability-reporting lookup returns HTTP 404. These
settings must be completed when eligible, immediately after changing visibility:

1. Enable private vulnerability reporting and confirm the **Report a
   vulnerability** entry point works. Until then, `SECURITY.md` directs reporters
   to request a private channel through the maintainer's profile.
2. Protect `main`: require pull requests and the successful CI/Security status
   checks; block force pushes and branch deletion. Set review requirements
   appropriate to the number of active maintainers.
3. Enable secret scanning, push protection, and CodeQL default setup for Go and
   Python. Confirm scheduled analyses and dependency alerts are running.
4. Verify anonymous README links, clone instructions, and license recognition.

Changing repository visibility and publishing tags, packages, or GitHub releases
are explicit maintainer actions. The build workflow only uploads candidate
artifacts; it does not publish to a registry.

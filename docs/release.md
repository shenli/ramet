# Preparing a release

Ramet is an Apache-2.0 development alpha. Release artifacts are built locally or
through the manually dispatched artifact workflow; no registry publication is
performed by the normal build or test commands.

1. Run `make check`, the native Linux Phase 2 gate, and
   `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` with the patched toolchain.
   Provider/FUSE tests must actually run, not skip. Record the soak's duration,
   source hashes, backend, cache/workload limits and resource trends.
2. Reconcile the changelog, SDK/CLI versions, status page and operations contract.
   Keep historical measurements intact and distinguish tested facts from scale
   targets. Check documentation links and Go formatting.
3. Build with `bash scripts/build-release.sh` and
   `uv build sdk/python --out-dir .test-cache/python-dist`. Inspect archive and
   wheel contents, install the wheel in a clean environment, and run SDK tests
   without a source-tree `PYTHONPATH`.
4. Build the Docker image and exercise its non-root lifecycle. Keep LICENSE,
   NOTICE and third-party texts in both binaries and images. Update notices for
   any newly linked dependency.
5. Check `git diff --check` and the complete staged diff. Confirm
   `git ls-files docs/ramet-marketing-internal.md docs/phase-1-plan.md docs/phase-2-plan.md`
   is empty and that private files,
   credentials, test caches and provider volumes are absent from artifacts.
   Ignoring or deleting a tracked file does not remove older versions from Git
   history. Review all refs before changing repository visibility.
6. Commit the reviewed source, run hosted CI, and select a release tag when
   ready to publish. Publishing tags, GitHub releases, images or the SDK is a
   separate operator action.

The release workflow uploads build candidates and checksums as workflow
artifacts. It has read-only repository permissions and does not publish packages.
Cross-built architectures are compilation evidence; only platforms exercised in
the validation report have runtime evidence.

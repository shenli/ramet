# Persistence policies and verified reconstruction

Use policies to explain which workspace state must survive and which can be
recreated. Ramet preserves unknown paths by default. Rules use relative path
prefixes, with the longest match winning; they are not globs.

## Choose the save operation

| Operation | What is saved | Recorded mode |
| --- | --- | --- |
| `snapshot(source=...)` / Git import | All supported files needed for the prepared base | `base` |
| `execution.checkpoint()` | All supported current files, including paths classified ephemeral | `checkpoint` |
| `execution.promote()` | Current files filtered by policy; immutable inputs must match the base | `promotion` |

Each new snapshot contains `manifest.save.mode`, an `excluded` list for that
publication, and a `rebuild` plan. The report is part of the content-addressed
manifest. An exclusion records path, class, kind, and logical byte count, not
file contents. `.git` cannot be excluded. Published snapshots do not change
when the execution continues writing.

A full checkpoint of a base with pending reconstruction steps carries those
steps forward conservatively. Capturing existing files does not prove that the
application completed reconstruction. A fresh full base capture records no
reconstruction requirement. Older manifests have no save-mode report; the plan
reports `legacy_unknown` and refuses to infer readiness for rebuildable rules.

## Preview a promotion

Stop external writers, then inspect the policy before saving:

```python
preview = execution.preview_promotion()
for item in preview["paths"]:
    print(item["path"], item["class"], item["action"], item["bytes"])
if not preview["valid"]:
    raise RuntimeError(preview["violations"])
saved = execution.promote()
```

The CLI exposes the same inspection:

```sh
ramet preview-promotion --execution execution.json
ramet restore-plan --id SNAPSHOT_ID
```

Decisions are `preserve`, `discard`, `rebuild`, or `retain_parent` when an
otherwise excluded directory contains a retained descendant. Violations report
immutable additions, edits and deletions, unsupported retained file types,
unsafe symlinks, or invalid recipe inputs. The CLI exits nonzero for preview
violations while still printing the report.

Preview publishes no snapshots or content. It reads retained file bytes and
can hydrate a lazy workspace's local cache. Totals are **logical file bytes**,
not network traffic, physical storage savings, or deduplicated object sizes.
When `optimized_git` is true, object packs are excluded from the estimate;
Git object integrity is checked during actual publication. A successful preview
is advisory: promotion scans again and can still fail. Keep writers stopped
between preview and publication.

## Declare reconstruction requirements

A rebuildable rule can declare regular-file inputs and a runtime identity:

```json
{
  "rules": [
    {
      "path": "node_modules",
      "class": "rebuildable",
      "recipe": "npm ci",
      "inputs": ["package.json", "package-lock.json"],
      "runtime": "your-reviewed-node-image@sha256:YOUR_IMAGE_DIGEST"
    },
    {"path": "scratch", "class": "ephemeral"},
    {"path": "scratch/reports", "class": "durable"},
    {"path": "fixtures", "class": "shared_immutable"}
  ]
}
```

On promotion, declared inputs must be retained regular files. Ramet records
their SHA-256 digests. Each step's ID binds its path, recipe, input digests, and
runtime string. Changing any of them changes the approval identity. The runtime
string is a caller-supplied requirement, not an attestation: your adapter must
check and use the intended image/toolchain. A recipe need not be a shell command.

`ramet.restore_plan(snapshot)` returns the saved requirements without acquiring
an execution. `execution.restore_plan()` additionally checks declared input
bytes on the worker. Symlinks, including symlink ancestors, are rejected for
these inputs. `valid` means the contract and inputs match, **not** that the
workspace is ready. Stop writers while inspecting these inputs.

## Reconstruct through your runtime

The Python SDK never executes recipe strings. Supply a handler keyed by an
explicitly approved step ID and an application readiness check:

```python
plan = ramet.restore_plan(saved)
# Review this plan against your application's known recipes, input hashes,
# and runtime before constructing the mapping. Do not auto-approve remote text.
approved_handlers = {approved_step_id: rebuild_dependencies}

with ramet.execution_pool(saved, concurrency=1) as pool:
    with pool.acquire() as execution:
        report = execution.rebuild(
            approved_handlers,
            verify=check_application_ready,
        )
        assert report["ready"]
```

`rebuild_dependencies(execution, step)` is your callback. It can call
`DockerRuntime.run(execution, ["npm", "ci"], check=True)` if dependencies are
available without network, or use your existing runtime with an appropriate
network policy. The supplied Docker adapter disables networking. Callbacks must
stop all writers before returning and must raise on failure, return `False`, or
return a failing `CompletedProcess` to signal failure. `None` is accepted for
callbacks that raise on failure. Do not pass a shell interpreter arbitrary
snapshot metadata.

All steps must have approved handlers before any runs. Steps execute in sorted
path order; there is no dependency scheduler. Use one recipe for interdependent
reconstruction work. Input hashes are checked before each step and before and
after the verifier. The verifier must return exactly `True`; successful command
exit alone does not establish readiness. It should check useful outputs,
retained agent work, and any required application tests. Recipes can still vary
with external services and platform details; hashes alone do not prove a
reproducible environment.

A failed step or verifier raises `RebuildError`; its `report` includes completed
step IDs, the failed step when applicable, and `ready: false`. Inspect the chained
exception for the runtime failure. No automatic retry occurs, and partial
outputs remain for inspection until release/reset. Restart from a clean
execution if retrying. The successful report is a point-in-time application
check, not a persisted readiness flag. Custom runtime cleanup must follow the
[integration contract](integration.md).

## Examples

- [Pinned image and reusable dependencies](session-recovery.md): retain dependency
  bytes as durable state, check runtime/input compatibility, and rebuild on mismatch.
- [Selective save and restore](../examples/persistence/README.md): executable
  walkthrough, Docker option, readiness check, and input-drift rejection.
- [Python policy](../examples/policies/python.json): opt-in `.venv` reconstruction
  from `requirements.lock`, root `__pycache__` exclusion, and retained reports.
- [Node policy](../examples/policies/node.json): opt-in dependency reconstruction
  from `package.json` and `package-lock.json`, plus retained reports.

Review these policies before attaching them to a snapshot. They are tested for
selection semantics; they do not include package-install handlers or certify
arbitrary projects. Python's example expects a hash-pinned requirements file.
Root path rules do not match every nested `__pycache__`; add explicit prefixes
as needed. Pin and record the runtime, and keep important generated outputs
under durable paths. No policy performs secret detection.

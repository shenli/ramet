# Resume work with a pinned image and persistent dependencies

Keep the runtime reproducible and the workspace recoverable. A replacement
sandbox starts the same image, attaches a restored workspace, checks its
dependencies, and continues the task. Processes restart; their memory is not
part of the checkpoint.

| State | Where it lives | Recovery |
| --- | --- | --- |
| OS, interpreters, system packages, document converters, system fonts | Image pinned by digest | Launch the same image and platform |
| Documents, inputs, intermediate work, task files | Durable workspace paths | Restore a Ramet snapshot |
| Workspace-local dependencies | A dedicated durable directory, such as `.deps` | Reuse after compatibility and readiness checks; rebuild on mismatch |
| Scratch files and disposable caches | Ephemeral workspace paths | Omit on promotion |
| Conversation state and task position | Your harness database | Record alongside the snapshot ID |

Put system dependencies into the image. Files installed outside the workspace
are not captured. Fonts or downloaded assets can instead live in durable
workspace paths when the tool supports an explicit search path.

## Prepare dependencies before running tools

The Python SDK provides `prepare_dependencies`. The SDK and worker must have
access to the same execution path; an HTTP connection alone does not provide
this filesystem access. The included Docker adapter mounts it at `/workspace`.

```python
from ramet import DockerRuntime, prepare_dependencies

# Select this from trusted application configuration, not workspace metadata.
runtime = DockerRuntime("registry.example.com/document-tools@sha256:" + IMAGE_DIGEST)
environment = runtime.dependency_environment()

with ramet.execution_pool(saved_snapshot, concurrency=1) as pool:
    with pool.acquire() as execution:
        report = prepare_dependencies(
            execution,
            environment,
            directory=".deps",
            inputs=["requirements.lock", "install-config.json"],
            rebuild=install_dependencies,  # Your trusted callback; replaces only .deps.
            verify=check_document_tools,    # Must return exactly True to establish readiness.
        )
        print("Reused dependencies:", report["reused"])
        # Run the next task only after preparation succeeds.
        runtime.run(execution, ["python", "continue_task.py"], check=True)
        saved = execution.promote()
```

See the [complete DOCX example](../examples/document_session/README.md) for the
callbacks, image build, save record, and worker replacement. Docker runs offline;
that example installs hash-pinned packages from a wheelhouse baked into the image.
It uses `pip --target=.deps`, avoiding virtualenv interpreter symlinks that point
outside the captured workspace.

The reuse identity includes:

- The exact image reference with a full SHA-256 digest.
- OS, architecture, and optional platform variant.
- The absolute workspace mount path inside the runtime.
- The dependency directory and SHA-256 of every declared input file.

Include lockfiles, installation scripts, and configuration that affect the
dependency build. Even a comment-only input change causes a conservative
rebuild. Environment variables, GPU/driver requirements, host features, and
external package sources are not detected automatically: pin them in declared
configuration and check them in your verifier where applicable. Avoid caches
that embed worker-host paths.

`DockerRuntime.dependency_environment()` rejects image tags and inspects the
pre-pulled image on the local daemon to obtain its platform. For a custom
runtime, construct `DependencyEnvironment(image=..., platform=...,
workspace_path=...)` from the runtime you actually launch. This is a
caller-verified compatibility contract, not remote attestation. Use a registry
digest for cross-host deployment; a local `sha256:...` image ID requires the
same image to be made available separately on the replacement host.

## Reuse, rebuild, and save

The helper checks `.deps/.ramet-environment.json` against the requested identity
and always calls the application verifier before reuse. Missing, malformed, or
mismatched metadata triggers the trusted builder. A verifier returning `False`
also triggers rebuild; an exception aborts preparation. A failed builder,
failed final verifier, or changed input aborts preparation without publishing a
new compatibility stamp. No retry or snapshot publication happens automatically.

The builder must replace stale dependencies inside its exclusively owned
directory, raise on error or return a failing `CompletedProcess`/`False`, and
leave user documents alone. The helper does not delete the directory itself.
Both callbacks must stop their writers before returning. SDK-owned runtime
cleanup and lease renewal are checked around callbacks; stop any external
writers before calling the helper. These filesystem checks are not a security
boundary against concurrently running or malicious processes.

The marker is a cache hint, not proof that packages are trustworthy or that the
application works. Restore only snapshots your application trusts, and make
the verifier exercise the tools you need. Metadata never supplies executable
commands. Package integrity and functional checks belong in your build and
verification callbacks.

Keep the dependency directory **durable** when you want it to survive
`promote()`. Marking it `rebuildable` intentionally omits its bytes and marker
on promotion; use the existing [approved reconstruction workflow](persistence.md)
for that policy. A full `checkpoint()` preserves supported files regardless of
ephemeral/rebuildable classifications. Preparation itself only changes local
files; call capture or promotion explicitly to persist them.

## Record the session and restore elsewhere

After publishing the workspace, store a record in your application's database:

```python
record = {
    "snapshot": saved.id,
    "environment": environment.to_dict(),
    "task_checkpoint": your_harness_checkpoint_id,
}
```

On recovery, validate the record against your application's approved images,
launch that runtime, restore the snapshot into a new execution, and prepare
dependencies before resuming tools. To intentionally change the image, use the
new trusted environment identity: old dependency metadata will miss and the
builder must produce compatible dependencies. Documents remain in the workspace;
their application-level compatibility still needs validation.

Publishing a Ramet snapshot and committing harness state are separate
operations. Coordinate a task boundary and stop writers to pair consistent
states; there is no cross-service atomic checkpoint. Preserve operation IDs
when reconciling uncertain snapshot publication rather than repeating it with
a new ID.

Local storage supports restart while the worker disk survives. PostgreSQL and
S3-compatible storage support restoration into an empty replacement worker
directory. The runtime must also have the pinned image available. This workflow
does not schedule sandboxes, transfer images, snapshot rootfs changes, or restore
process memory. Cold restore still transfers uncached content; dependency reuse
avoids installation, not downloading the saved dependency files.

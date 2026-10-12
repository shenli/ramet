# Selective save and verified restore

This runnable example shows what a persistence policy saves, what it omits, and
how an application proves a reconstructed workspace is usable. It generates its
own small project and starts a temporary worker; no model API or package download
is needed. The temporary store is deleted on exit.

From the repository root, with Go 1.26.9+ and Python 3.10+:

```sh
make build
PYTHONPATH=sdk/python python3 examples/persistence/run.py
```

The default executes only the example's generated, trusted Python build script
on the host. To reconstruct inside the included Docker adapter instead:

```sh
docker pull python:3.12-slim-bookworm
PYTHONPATH=sdk/python python3 examples/persistence/run.py --docker
```

The SDK, worker, and local Docker daemon must resolve the same filesystem paths.
Use `--image` for another pre-pulled Python image; pin its digest when validating
a specific runtime. This example uses the copy/reflink backend.

The workflow:

1. Prepare a project with a schema, build script, generated output, and scratch logs.
2. Acquire a workspace, add an agent result, and print the per-path promotion preview.
3. Promote the result while excluding generated output and scratch files.
4. Reacquire from the original base and verify no agent result leaked into it.
5. Restore the saved snapshot, approve a known build recipe and exact input hashes,
   and run the recipe through a callback.
6. Verify both rebuilt output and preserved agent work before reporting readiness.
7. Modify a recipe input and confirm that reconstruction is rejected.

Expected final line:

```text
PASS: selective save, clean recycle, verified reconstruction, input-drift rejection
```

The same snapshot can restore an interactive agent's saved work in a replacement
sandbox or supply the base for another batch of attempts. Process memory and
external effects are outside this filesystem workflow.

The fixture demonstrates generated-file reconstruction, not a package-manager
benchmark. See [persistence policies](../../docs/persistence.md) for the API,
Python and Node policy examples, and failure behavior.

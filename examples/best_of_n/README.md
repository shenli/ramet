# Best-of-N coding workflow

This example fixes discount/shipping logic in a tiny checkout module. Three
deterministic candidate patches run concurrently in separate Docker containers.
Six unit tests select exactly one passing candidate. Ramet promotes that
candidate, verifies a recycled slot is clean, then restores the selected
snapshot into a new pool and reruns the tests.

From the repository root, with Go, Git, Python 3.10+, and local Docker installed:

```sh
make build
python3 -m pip install ./sdk/python
docker pull python:3.12-slim-bookworm
python3 examples/best_of_n/run.py --output .ramet/coding-demo
git -C .ramet/coding-demo/source apply --check ../winner.patch
```

Use a new output directory for every run. Artifacts include `winner.patch`,
`report.json`, candidate logs, and the stopped worker's durable `store/`.
The report records actual preparation/recycle timing, cache counters, and
verification outcomes. It is a functional demo, not a scale benchmark. The
fixture intentionally starts with failing tests; it is not part of the SDK suite.

The default image is pulled explicitly before execution. For reproducible runs,
pass `--image python@sha256:YOUR_VERIFIED_DIGEST`. The SDK never pulls images
implicitly, forwards host credentials, or enables workload networking.

To integrate your own agent, replace the candidate-generation step with your
agent harness. Run its commands through your runtime, stop every writer, run an
independent verifier, and promote only the state you want to retain. Do not give
the workload the worker data directory or Docker socket. The example's
`git diff --binary` exports its tracked-file fix; this is not a general snapshot
export API and does not include untracked files or filesystem metadata.

State can be reopened with `ramet serve --data .ramet/coding-demo/store`.
The selected snapshot ID is in `report.json`. Worker processes and pools are
ephemeral; only promoted snapshots survive.

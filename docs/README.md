# Ramet documentation

Ramet creates isolated writable executions from immutable filesystem snapshots.
These guides describe the **Phase 2 implementation**; the product design also
contains future capabilities. Start with the guide that matches your task.

| Task | Guide |
| --- | --- |
| Integrate an agent harness with verifiable recovery | [Agent integration guide](agents.md) |
| Give SkyRL rollouts isolated workspaces | [SkyRL integration and runnable environment](skyrl-integration.md) |
| Preview saved state and reconstruct omitted files | [Persistence policies](persistence.md), [runnable example](../examples/persistence/README.md) |
| Assess integration with Google AX | [AX assessment](ax-integration.md) |
| Estimate and implement integration with your framework or sandbox | [Integration guide](integration.md) |
| Try competing coding fixes and preserve a passing result | [Best-of-N demo](../examples/best_of_n/README.md) |
| Understand components, storage, and deployment boundaries | [Architecture](architecture.md) |
| Understand lifecycle semantics and implementation tradeoffs | [Implementation design](design.md) |
| Build, test, debug, or extend Ramet | [Developer guide](development.md) |
| Configure a worker, integrate the API, or recover storage | [Operations](operations.md) |
| Hydrate workspace files on first access | [Linux FUSE setup](fuse.md) |
| Integrate a Python or Go harness | [Python SDK](../sdk/python/README.md), [Go integration](go.md), and [example](../examples/python/parallel_eval.py) |
| Evaluate performance claims or reproduce measurements | [Benchmarking](benchmarking.md) and [validation](validation.md) |
| Check what exists and what comes next | [Implementation status](implementation-status.md) |
| Read the long-term product direction | [Product and technical design](ramet-product-technical-design.md) |
| Build or review a release candidate | [Release checklist](release.md), [prelaunch checklist](prelaunch.md), and [changelog](../CHANGELOG.md) |
| Contribute or report a security issue | [Contributing](../CONTRIBUTING.md) and [security policy](../SECURITY.md) |

For a first code change, read architecture, then implementation design, then
follow the developer guide's local test loop. For runtime integration, read the
operations correctness contract and security policy before mounting execution
paths into workloads.

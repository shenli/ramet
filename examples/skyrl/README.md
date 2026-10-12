# SkyRL workspace environment

Follow the [SkyRL integration guide](../../docs/skyrl-integration.md) for setup,
the no-GPU smoke test, registration, dataset fields, and deployment requirements.

- `ramet_env.py`: a `BaseTextEnv` template using one Ramet workspace per
  trajectory and offline Docker commands. Adapt its action parser and toy grader.
- `test_smoke.py`: real SkyRL-Gym, Ramet HTTP, and Docker lifecycle checks.

This example requires a colocated copy-backend worker and local Docker daemon.
It does not provide distributed placement or a crash-recovery supervisor.

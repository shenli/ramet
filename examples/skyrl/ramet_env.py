# SPDX-License-Identifier: Apache-2.0
"""SkyRL-Gym example: one copy workspace per trajectory, offline Docker tools.

This is an integration template, not a training benchmark or a cluster scheduler.
See docs/skyrl-integration.md for placement, cancellation, and recovery requirements.
"""
from __future__ import annotations

import json

from ramet import DockerRuntime, Ramet
from skyrl_gym.envs.base_text_env import BaseTextEnv


class RametCommandEnv(BaseTextEnv):
    def __init__(self, env_config=None, extras=None):
        super().__init__()
        config, extras = env_config or {}, extras or {}
        self.base = extras["extra_info"]["ramet_snapshot_id"]
        self.expected = str(extras["reward_spec"]["ground_truth"])
        self.max_turns = int(extras.get("max_turns", 8))
        if self.max_turns < 1:
            raise ValueError("max_turns must be positive")
        self.timeout = float(config.get("command_timeout", 30))
        self.ramet = Ramet()  # RAMET_URL / RAMET_TOKEN belong to the harness.
        self.runtime = DockerRuntime(config.get("image", "python:3.12-slim-bookworm"))
        self.pool = None
        self.execution = None
        self.done = False
        self.closed = False

    def init(self, prompt):
        if self.closed or self.pool is not None:
            raise RuntimeError("create a new environment for each trajectory")
        # Keep handles before starting renewal so close() can clean up if init fails.
        self.pool = self.ramet.execution_pool(self.base, concurrency=1)
        self.execution = self.pool.acquire()
        self.execution.__enter__()  # Renew throughout model generation and tools.
        if not self.execution.verify()["valid"]:
            raise RuntimeError("initial workspace verification failed")
        return prompt, {}

    def step(self, action):
        if self.execution is None or self.closed or self.done:
            raise RuntimeError("step requires an active, unfinished trajectory")
        self.turns += 1
        try:
            request = json.loads(action)
            if not isinstance(request, dict):
                raise ValueError("expected an object")
            if set(request) - {"argv", "finish"}:
                raise ValueError("unknown action field")
            finish = request.get("finish", False)
            argv = request.get("argv")
            if type(finish) is not bool:
                raise ValueError("finish must be a boolean")
            if argv is not None and (not isinstance(argv, list) or not argv or
                                     not all(isinstance(x, str) for x in argv)):
                raise ValueError("argv must be a nonempty list of strings")
            if argv is None and not finish:
                raise ValueError("provide argv or finish=true")
        except (ValueError, TypeError) as error:
            self.done = self.turns >= self.max_turns
            return dict(observations=[{"role": "user", "content": str(error)}],
                        reward=0.0, done=self.done, metadata={})

        output = ""
        if argv is not None:
            result = self.runtime.run(self.execution, argv, timeout=self.timeout)
            output = f"exit={result.returncode}\n{result.stdout}\n{result.stderr}"[-8192:]
        self.done = finish or self.turns >= self.max_turns
        reward = 0.0
        if self.done:
            # Toy task: result.txt must equal ground_truth. The grader is supplied
            # by the harness, outside writable task files, and runs in isolation.
            grader = (
                "import pathlib,sys; p=pathlib.Path('result.txt'); "
                "sys.exit(0 if p.is_file() and "
                "p.read_text() == sys.argv[1] else 1)"
            )
            result = self.runtime.run(self.execution,
                                      ["python", "-I", "-c", grader, self.expected],
                                      timeout=self.timeout)
            reward = float(result.returncode == 0)
        # Infrastructure errors propagate; they are not incorrect-answer rewards.
        return dict(observations=[{"role": "user", "content": output or "finished"}],
                    reward=reward, done=self.done, metadata={})

    def close(self):
        # Failed stop/release must prevent pool deletion. Retain handles so a
        # supervisor can inspect the failure; never suppress cleanup errors.
        if self.execution is not None:
            self.execution.release()  # Stops registered Docker runtime + renewal.
            self.execution = None
        if self.pool is not None:
            self.pool.close()
            self.pool = None
        self.closed = True

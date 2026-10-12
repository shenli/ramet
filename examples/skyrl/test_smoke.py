# SPDX-License-Identifier: Apache-2.0
"""Requires a same-host copy worker, SkyRL-Gym, and a pre-pulled Docker image.

Run with: python -m unittest discover -s examples/skyrl -p test_smoke.py -v
"""
import json
import tempfile
import unittest
from pathlib import Path
from subprocess import TimeoutExpired
from unittest.mock import patch

import skyrl_gym
from ramet import Ramet

from ramet_env import RametCommandEnv


class SkyRLSmokeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ramet = Ramet()
        if cls.ramet.capabilities()["backend"] != "copy":
            raise RuntimeError("this Docker example requires a copy worker")
        with tempfile.TemporaryDirectory(prefix="ramet-skyrl-source-") as source:
            (Path(source) / "base.txt").write_text("unchanged")
            cls.base = cls.ramet.snapshot(source, policy={"rules": [
                {"path": "scratch", "class": "ephemeral"},
            ]})
        skyrl_gym.register(id="ramet_command_smoke", entry_point=RametCommandEnv)

    def make_env(self, **config):
        env = skyrl_gym.make("ramet_command_smoke", env_config=config, extras={
            "extra_info": {"ramet_snapshot_id": self.base.id},
            "reward_spec": {"ground_truth": "ramet"}, "max_turns": 3,
        })
        self.addCleanup(env.close)
        env.init([{"role": "user", "content": "Write ramet to result.txt."}])
        return env

    def test_isolation_promotion_and_fresh_episode(self):
        first, second = self.make_env(), self.make_env()
        result = first.step(json.dumps({"argv": ["python", "-c",
            "from pathlib import Path; Path('result.txt').write_text('ramet'); "
            "Path('scratch').mkdir(); Path('scratch/tmp').write_text('omit')"],
            "finish": True}))
        self.assertEqual(result["reward"], 1.0)
        self.assertTrue(result["done"])
        self.assertFalse((second.execution.path / "result.txt").exists())
        self.assertTrue(second.execution.verify()["valid"])
        # Selection belongs to the harness, before close releases the workspace.
        saved = first.execution.promote()
        first.close()
        first.close()  # Successful close is idempotent.
        with self.ramet.execution_pool(saved, concurrency=1) as pool:
            with pool.acquire() as restored:
                self.assertEqual((restored.path / "result.txt").read_text(), "ramet")
                self.assertFalse((restored.path / "scratch").exists())
        fresh = self.make_env()
        self.assertFalse((fresh.execution.path / "result.txt").exists())

    def test_bad_action_and_turn_limit(self):
        env = self.make_env()
        self.assertFalse(env.step("not JSON")["done"])
        self.assertFalse(env.step('{"finish": "yes"}')["done"])
        result = env.step('{"finish": true}')
        self.assertTrue(result["done"])
        self.assertEqual(result["reward"], 0.0)
        with self.assertRaises(RuntimeError):
            env.step('{"finish": true}')

    def test_timeout_stops_runtime_before_close(self):
        env = self.make_env(command_timeout=2)
        with self.assertRaises(TimeoutExpired):
            env.step(json.dumps({"argv": ["python", "-c", "import time; time.sleep(60)"]}))
        env.close()

    def test_cleanup_failure_blocks_pool_deletion(self):
        env = self.make_env()
        with patch.object(env.execution, "release", side_effect=RuntimeError("cleanup failed")):
            with patch.object(env.pool, "close") as close_pool:
                with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
                    env.close()
                close_pool.assert_not_called()
        env.close()

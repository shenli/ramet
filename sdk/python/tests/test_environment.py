# SPDX-License-Identifier: Apache-2.0
from dataclasses import replace
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

from ramet import DependencyEnvironment, DockerRuntime, prepare_dependencies


class DependencyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "requirements.lock").write_text("package==1")
        (self.root / "report.docx").write_bytes(b"valuable work")
        self.execution = Mock(path=self.root, _lock=threading.RLock())
        self.environment = DependencyEnvironment("registry/example@sha256:" + "a" * 64, "linux/amd64")
        self.marker = self.root / ".deps/.ramet-environment.json"
        self.rebuild = Mock(side_effect=self.build)

    def build(self, execution):
        (execution.path / ".deps").mkdir(exist_ok=True)
        (execution.path / ".deps/installed").write_text("ready")

    def prepare(self, **kwargs):
        options = dict(directory=".deps", inputs=["requirements.lock"],
                       rebuild=self.rebuild, verify=lambda e: (e.path / ".deps/installed").is_file())
        options.update(kwargs)
        environment = options.pop("environment", self.environment)
        return prepare_dependencies(self.execution, environment, **options)

    def test_reuse_and_each_compatibility_dimension(self):
        self.assertFalse(self.prepare()["reused"])
        self.assertTrue(self.prepare()["reused"])
        self.assertEqual(self.rebuild.call_count, 1)
        for environment in (
                replace(self.environment, image="sha256:" + "b" * 64),
                replace(self.environment, platform="linux/arm64/v8"),
                replace(self.environment, workspace_path="/work")):
            self.prepare()
            self.assertFalse(self.prepare(environment=environment)["reused"])
        self.prepare()
        (self.root / "requirements.lock").write_text("package==2")
        self.assertFalse(self.prepare()["reused"])
        self.assertEqual((self.root / "report.docx").read_bytes(), b"valuable work")

    def test_bad_marker_missing_dependencies_and_readiness_failure(self):
        self.prepare()
        self.marker.write_text("not json")
        self.assertFalse(self.prepare()["reused"])
        (self.root / ".deps/installed").unlink()
        self.assertFalse(self.prepare()["reused"])
        with self.assertRaisesRegex(RuntimeError, "readiness"):
            self.prepare(verify=lambda e: False)
        self.assertFalse(self.marker.exists())
        self.assertFalse(self.prepare()["reused"])

    def test_failed_callbacks_and_input_drift_leave_no_stamp(self):
        for callback in (Mock(side_effect=RuntimeError("install failed")),
                         Mock(return_value=False),
                         Mock(return_value=subprocess.CompletedProcess([], 1))):
            self.prepare()
            with self.assertRaises(RuntimeError):
                self.prepare(environment=replace(self.environment, platform="linux/arm64"), rebuild=callback)
            self.assertFalse(self.marker.exists())
        self.prepare()
        def changed(execution):
            (execution.path / "requirements.lock").write_text("unexpected change")
            return True
        with self.assertRaisesRegex(RuntimeError, "inputs changed"):
            self.prepare(verify=changed)
        self.assertFalse(self.marker.exists())
        self.prepare()
        with self.assertRaisesRegex(RuntimeError, "verifier failed"):
            self.prepare(verify=Mock(side_effect=RuntimeError("verifier failed")))
        self.assertFalse(self.marker.exists())

    def test_paths_and_pins_fail_closed(self):
        for path in ("..", "../outside", "/tmp/outside", ".", ".git", "a/../b", "a//b"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.prepare(directory=path)
        for inputs in ([], "requirements.lock", [".deps/lock"], ["requirements.lock"] * 2):
            with self.assertRaises(ValueError):
                self.prepare(inputs=inputs)
        for image in ("python:latest", "sha256:abc", "repo@sha256:" + "A" * 64):
            with self.assertRaises(ValueError):
                replace(self.environment, image=image)
        for path in ("workspace", "/a/../b", "/a//b"):
            with self.assertRaises(ValueError):
                replace(self.environment, workspace_path=path)
        self.rebuild.assert_not_called()

    def test_symlink_inputs_directories_and_markers_rejected(self):
        (self.root / "linked.lock").symlink_to("requirements.lock")
        with self.assertRaises(ValueError):
            self.prepare(inputs=["linked.lock"])
        (self.root / "actual").mkdir()
        (self.root / ".deps").symlink_to("actual", target_is_directory=True)
        with self.assertRaises(ValueError):
            self.prepare()
        (self.root / ".deps").unlink()
        (self.root / ".deps").mkdir()
        self.marker.symlink_to("../report.docx")
        with self.assertRaises(ValueError):
            self.prepare()
        self.assertEqual((self.root / "report.docx").read_bytes(), b"valuable work")
        self.rebuild.assert_not_called()

    def test_cleanup_and_lease_failure_prevent_preparation(self):
        for method in (self.execution.stop_runtime, self.execution.heartbeat):
            method.side_effect = RuntimeError("runtime or lease unavailable")
            with self.assertRaises(RuntimeError):
                self.prepare()
            self.rebuild.assert_not_called()
            method.side_effect = None
        self.execution._heartbeat_thread = None
        with self.assertRaisesRegex(RuntimeError, "pool.acquire"):
            self.prepare()

    def test_docker_identity_uses_daemon_platform_and_requires_pin(self):
        with patch("ramet.docker.subprocess.run") as run, patch.dict("os.environ", {}, clear=True):
            with self.assertRaises(ValueError):
                DockerRuntime("python:latest").dependency_environment()
            run.assert_not_called()
            run.side_effect = [Mock(stdout="unix:///docker.sock"), Mock(stdout=json.dumps([
                {"Os": "linux", "Architecture": "arm64", "Variant": "v8"}]))]
            environment = DockerRuntime(self.environment.image).dependency_environment()
            self.assertEqual(environment.platform, "linux/arm64/v8")
            self.assertEqual(environment.image, self.environment.image)
            self.assertEqual(environment.workspace_path, "/workspace")

# SPDX-License-Identifier: Apache-2.0
"""Real worker integration tests. Build bin/ramet first (make build)."""
import concurrent.futures
import json
import socket
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, urlopen
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest

from ramet import (DependencyEnvironment, DockerRuntime, Ramet, RametEnv, RametError,
                   OutcomeUnknownError, RebuildError, prepare_dependencies)


class IntegrationTests(unittest.TestCase):
    def setUp(self):
        failures = []
        original = threading.excepthook
        threading.excepthook = failures.append
        self.addCleanup(setattr, threading, "excepthook", original)
        self.addCleanup(lambda: self.assertEqual(
            failures, [], "an SDK background thread crashed"))

    @classmethod
    def setUpClass(cls):
        binary = Path(os.getenv("RAMET_TEST_BINARY", str(Path(__file__).resolve().parents[3] / "bin/ramet")))
        if not binary.exists():
            raise RuntimeError(f"build the worker first: make build (missing {binary})")
        cls.temp = tempfile.TemporaryDirectory(prefix="ramet-python-test-")
        cls.addClassCleanup(cls.temp.cleanup)
        root = Path(cls.temp.name)
        cls.source = root / "source"
        cls.source.mkdir()
        (cls.source / "base.txt").write_text("base")
        cls.log = (root / "worker.log").open("w+")
        cls.addClassCleanup(cls.log.close)
        backend = os.getenv("RAMET_TEST_BACKEND", "copy")
        if backend not in ("copy", "lazy"):
            raise ValueError("RAMET_TEST_BACKEND must be copy or lazy")
        config = root / "worker.json"
        config.write_text(json.dumps({"worker": {"backend": backend}}))
        cls.proc = subprocess.Popen([str(binary), "serve", "--data", str(root / "store"),
                                     "--listen", "127.0.0.1:0", "--config", str(config)],
                                    env={**os.environ, "RAMET_TOKEN": "test-secret"},
                                    stdout=subprocess.DEVNULL, stderr=cls.log)
        def stop_worker():
            if cls.proc.poll() is None:
                cls.proc.terminate()
            cls.proc.wait(timeout=20)
        cls.addClassCleanup(stop_worker)
        for _ in range(100):
            cls.log.flush()
            cls.log.seek(0)
            lines = cls.log.readlines()
            for line in lines:
                try:
                    entry = json.loads(line)
                except json.JSONDecodeError:
                    continue  # startup failures are reported below, verbatim
                if entry.get("address"):
                    cls.client = Ramet("http://" + entry["address"], token="test-secret")
                    cls.base = cls.client.snapshot(cls.source)
                    return
            if cls.proc.poll() is not None:
                raise RuntimeError("worker exited: " + "".join(lines))
            time.sleep(.05)
        cls.proc.terminate()
        cls.proc.wait(timeout=5)
        raise RuntimeError("worker did not start")

    def test_lost_reset_response_reconciles_without_second_reset(self):
        real_url = self.client.url
        token = self.client.token
        class DropResponse(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                request = Request(real_url + self.path, data=body, method="POST", headers={
                    "Authorization": "Bearer " + token,
                    "Content-Type": "application/json",
                    "Idempotency-Key": self.headers["Idempotency-Key"],
                })
                with urlopen(request, timeout=5) as response:
                    response.read()  # The worker committed; discard its acknowledgement.
                self.connection.shutdown(socket.SHUT_RDWR)
                self.connection.close()
        with self.client.execution_pool(self.base, concurrency=1) as pool:
            with pool.acquire() as execution:
                before = execution._data["generation"]
                operation_id = self.client.new_operation()
                proxy = ThreadingHTTPServer(("127.0.0.1", 0), DropResponse)
                thread = threading.Thread(target=proxy.serve_forever, daemon=True)
                thread.start()
                try:
                    self.client.url = "http://127.0.0.1:" + str(proxy.server_port)
                    with self.assertRaises(OutcomeUnknownError) as caught:
                        execution.reset(operation_id=operation_id)
                    self.assertEqual(caught.exception.operation_id, operation_id)
                finally:
                    self.client.url = real_url
                    proxy.shutdown()
                    proxy.server_close()
                    thread.join()
                receipt = self.client.operation(operation_id)
                self.assertEqual(receipt["state"], "completed")
                self.assertEqual(receipt["status"], 200)
                execution.reset(operation_id=operation_id)
                self.assertEqual(execution._data["generation"], before + 1)
                self.assertTrue(execution.verify()["valid"])

    def test_error_correlation_and_capabilities(self):
        self.assertIn(self.client.capabilities()["backend"], ("copy", "lazy"))
        with self.assertRaises(RametError) as caught:
            self.client._request("GET", "/v1/missing")
        self.assertEqual(caught.exception.code, "endpoint_not_found")
        self.assertTrue(caught.exception.request_id)
        for policy in ({"rulez": []}, [], {"rules": None}):
            with self.assertRaises(RametError) as caught:
                self.client.snapshot(self.source, policy=policy)
            self.assertEqual(caught.exception.status, 400)
            self.assertTrue(caught.exception.request_id)

    def test_policy_preview_and_approved_reconstruction(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            (source / "lock.json").write_text("locked")
            (source / "deps").mkdir()
            (source / "deps/package").write_text("generated")
            (source / "scratch").write_text("temporary")
            base = self.client.snapshot(source, policy={"rules": [
                {"path": "deps", "class": "rebuildable", "recipe": "approved build",
                 "inputs": ["lock.json"], "runtime": "test-runtime"},
                {"path": "scratch", "class": "ephemeral"},
            ]})
            with self.client.execution_pool(base, concurrency=1) as pool:
                with pool.acquire() as execution:
                    count = len(self.client.snapshots())
                    preview = execution.preview_promotion()
                    self.assertTrue(preview["valid"])
                    self.assertEqual(preview["omitted_bytes"], len("generatedtemporary"))
                    self.assertEqual(len(self.client.snapshots()), count)
                    saved = execution.promote()
            plan = self.client.restore_plan(saved)
            self.assertEqual(plan["save_mode"], "promotion")
            step = plan["steps"][0]
            calls = []
            def reconstruct(execution, received):
                self.assertEqual(received, step)
                calls.append(received["id"])
                (execution.path / "deps").mkdir(exist_ok=True)
                (execution.path / "deps/package").write_text("generated")
            def ready(execution):
                return (execution.path / "deps/package").read_text() == "generated"
            with self.client.execution_pool(saved, concurrency=1) as pool:
                with pool.acquire() as execution:
                    self.assertFalse((execution.path / "deps").exists())
                    self.assertFalse((execution.path / "scratch").exists())
                    with self.assertRaises(RebuildError) as missing:
                        execution.rebuild({}, verify=ready)
                    self.assertFalse(missing.exception.report["ready"])
                    self.assertEqual(calls, [])
                    result = execution.rebuild({step["id"]: reconstruct}, verify=ready)
                    self.assertTrue(result["ready"])
                    self.assertEqual(result["completed"], [step["id"]])
                    (execution.path / "lock.json").write_text("changed")
                    with self.assertRaises(RebuildError):
                        execution.rebuild({step["id"]: reconstruct}, verify=ready)
                    self.assertEqual(calls, [step["id"]])

    def test_parallel_recycle_and_reset(self):
        with self.client.execution_pool(self.base, concurrency=4) as pool:
            def attempt(i):
                with pool.acquire() as execution:
                    marker = execution.path / "attempt"
                    self.assertFalse(marker.exists())
                    marker.write_text(str(i))
                    self.assertFalse(execution.verify()["valid"])
                    old_path = execution.path
                    execution.reset()
                    self.assertNotEqual(old_path, execution.path)
                    self.assertTrue(execution.verify()["valid"])
                    self.assertEqual((execution.path / "base.txt").read_text(), "base")
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
                list(executor.map(attempt, range(24)))

    def test_dependency_reuse_after_restore_and_policy_exclusion(self):
        environment = DependencyEnvironment("sha256:" + "a" * 64, "linux/amd64")
        builds = []
        def build(execution):
            builds.append(execution.id)
            (execution.path / "deps").mkdir(exist_ok=True)
            (execution.path / "deps/package").write_text("installed")
        def prepare(execution):
            return prepare_dependencies(execution, environment, directory="deps", inputs=["base.txt"],
                rebuild=build, verify=lambda e: (e.path / "deps/package").read_text() == "installed")
        for dependency_class in ("durable", "ephemeral"):
            with self.subTest(policy=dependency_class):
                base = self.client.snapshot(self.source, policy={"rules": [
                    {"path": "deps", "class": dependency_class}]})
                with self.client.execution_pool(base, concurrency=1) as pool:
                    with pool.acquire() as execution:
                        self.assertFalse(prepare(execution)["reused"])
                        (execution.path / "report.docx").write_bytes(b"retained artifact")
                        saved = execution.promote()
                before = len(builds)
                with self.client.execution_pool(saved, concurrency=1) as pool:
                    with pool.acquire() as execution:
                        self.assertEqual(prepare(execution)["reused"], dependency_class == "durable")
                        self.assertEqual(len(builds) - before, int(dependency_class != "durable"))
                        (execution.path / "base.txt").write_text("changed dependency inputs")
                        self.assertFalse(prepare(execution)["reused"])
                        self.assertEqual((execution.path / "report.docx").read_bytes(), b"retained artifact")

    def test_promote_resume_and_fork(self):
        with self.client.execution_pool(self.base, concurrency=2) as pool:
            with pool.acquire() as execution:
                (execution.path / "base.txt").write_text("changed")
                with execution.fork() as child:
                    self.assertEqual((child.path / "base.txt").read_text(), "changed")
                    (child.path / "base.txt").write_text("fork")
                    self.assertEqual((execution.path / "base.txt").read_text(), "changed")
                snap = execution.promote()
        with self.client.execution_pool(snap, concurrency=1) as pool:
            with pool.acquire() as execution:
                self.assertEqual((execution.path / "base.txt").read_text(), "changed")

    def test_env_and_local_run(self):
        with self.client.execution_pool(self.base, concurrency=1) as pool:
            with RametEnv(pool, lambda e: (e.path / "base.txt").read_text(),
                          lambda e, action: (e.path / "base.txt").write_text(action)) as env:
                self.assertEqual(env.reset(), "base")
                env.step("changed")
                self.assertEqual(env.state(), "changed")
                self.assertEqual(env.reset(), "base")
                result = env.execution.run(["/bin/sh", "-c", "cat base.txt"], check=True)
                self.assertEqual(result.stdout, "base")
                with self.assertRaises(subprocess.TimeoutExpired):
                    env.execution.run(["/bin/sh", "-c", "sleep 5"], timeout=.01)
                self.assertTrue(env.execution.verify()["valid"])

    def test_auth_and_exception_cleanup(self):
        with self.assertRaises(RametError) as caught:
            Ramet(self.client.url, token="wrong").snapshots()
        self.assertEqual(caught.exception.status, 401)
        with self.client.execution_pool(self.base, concurrency=1) as pool:
            with self.assertRaises(ValueError):
                with pool.acquire() as execution:
                    raise ValueError("failed evaluation")
            with pool.acquire() as next_execution:
                self.assertTrue(next_execution.verify()["valid"])
            with self.assertRaises(RuntimeError):
                execution.verify()
        self.assertTrue(self.client.doctor()["ok"])

    def test_prefetch_locality_and_warm(self):
        report = self.client.prefetch(self.base)
        self.assertEqual(report["missing_bytes"], 0)
        self.assertEqual(report["cached_objects"], report["objects"])
        with self.client.execution_pool(self.base, concurrency=2) as pool:
            warmed = pool.warm(2)
            self.assertEqual(warmed, {"prepared": 2, "idle": 2})
            self.assertEqual(self.client.locality(self.base)["idle_executions"], 2)
            with pool.acquire() as execution:
                self.assertTrue(execution.verify()["valid"])
        self.assertEqual(self.client.locality(self.base.id)["idle_executions"], 0)

    def test_cache_and_idle_controls(self):
        with self.client.execution_pool(self.base, concurrency=2) as pool:
            pool.warm(2)
            self.assertEqual(self.client.cache_status()["idle_executions"], 2)
            self.assertEqual(self.client.trim_idle(), {"removed": 2})
            self.assertEqual(self.client.cache_status()["idle_executions"], 0)
        with self.assertRaises(RametError) as failure:
            self.client.trim_cache()
        self.assertEqual(failure.exception.status, 409)  # local data is authoritative

    def test_batch_and_automatic_heartbeat(self):
        from contextlib import ExitStack
        with self.client.execution_pool(self.base, concurrency=4, lease_seconds=1) as pool:
            with ExitStack() as stack:
                executions = [stack.enter_context(e) for e in pool.acquire(count=4)]
                # Foreground command holds the lifecycle lock; renewals must
                # continue under the separate lease lock while it runs.
                executions[0].run(["/bin/sh", "-c", "sleep 2"], timeout=5, check=True)
                for execution in executions:
                    self.assertTrue(execution.verify()["valid"])
            with pool.acquire() as clean:
                self.assertTrue(clean.verify()["valid"])

    @unittest.skipUnless(os.getenv("RAMET_TEST_DOCKER") == "1", "set RAMET_TEST_DOCKER=1")
    def test_docker_timeout_isolation_and_recycle(self):
        runtime = DockerRuntime(os.getenv("RAMET_TEST_IMAGE", "python:3.12-slim-bookworm"))
        with self.client.execution_pool(self.base, concurrency=1, lease_seconds=2) as pool:
            with pool.acquire() as execution:
                result = runtime.run(execution, ["python", "-c",
                    "import pathlib,time; pathlib.Path('marker').write_text('container'); "
                    "time.sleep(3); print(pathlib.Path('base.txt').read_text())"], check=True)
                self.assertEqual(result.stdout.strip(), "base")
                denied = runtime.run(execution, ["python", "-c",
                    "open('/cannot-write-root','w').write('bad')"])
                self.assertNotEqual(denied.returncode, 0)
                with self.assertRaises(subprocess.TimeoutExpired):
                    runtime.run(execution, ["python", "-c",
                        "import time,subprocess; subprocess.Popen(['sleep','30']); time.sleep(30)"],
                        timeout=1)
                self.assertIsNone(execution._runtime_cleanup)
                self.assertFalse(pool._runtime_executions)
                self.assertEqual((execution.path / "marker").read_text(), "container")
            with pool.acquire() as clean:
                self.assertTrue(clean.verify()["valid"])
                self.assertFalse((clean.path / "marker").exists())

    @unittest.skipUnless(os.getenv("RAMET_TEST_DOCKER") == "1", "set RAMET_TEST_DOCKER=1")
    def test_docker_bounds_combined_output_and_recycles(self):
        runtime = DockerRuntime(os.getenv("RAMET_TEST_IMAGE", "python:3.12-slim-bookworm"),
                                max_output_bytes=1024)
        with self.client.execution_pool(self.base, concurrency=1) as pool:
            with pool.acquire() as execution:
                exact = runtime.run(execution, ["python", "-c",
                    "import os; os.write(1,b'a'*512); os.write(2,b'b'*512)"], check=True)
                self.assertEqual(exact.stdout, "a" * 512)
                self.assertEqual(exact.stderr, "b" * 512)
                with self.assertRaisesRegex(RuntimeError, "max_output_bytes"):
                    runtime.run(execution, ["python", "-c",
                        "import os,time; open('noisy','w').write('dirty'); "
                        "os.write(1,b'a'*512); os.write(2,b'b'*513); time.sleep(60)"], timeout=10)
                self.assertIsNone(execution._runtime_cleanup)
                self.assertFalse(pool._runtime_executions)
                # Binary output remains bounded and decodes without losing cleanup.
                binary = runtime.run(execution, ["python", "-c", "import os; os.write(1,b'\\xff')"])
                self.assertEqual(binary.stdout, "\ufffd")
            with pool.acquire() as clean:
                self.assertTrue(clean.verify()["valid"])
                self.assertFalse((clean.path / "noisy").exists())

    @unittest.skipUnless(os.getenv("RAMET_TEST_DOCKER") == "1", "set RAMET_TEST_DOCKER=1")
    def test_docker_stops_on_expired_lease(self):
        runtime = DockerRuntime(os.getenv("RAMET_TEST_IMAGE", "python:3.12-slim-bookworm"))
        with self.client.execution_pool(self.base, concurrency=1, lease_seconds=2) as pool:
            execution = pool.acquire().__enter__()
            # Model a stalled renewal loop while retaining the context handle.
            execution._heartbeat_stop.set()
            execution._heartbeat_thread.join()
            try:
                with self.assertRaisesRegex(RuntimeError, "lease expired"):
                    runtime.run(execution, ["python", "-c", "import time; time.sleep(30)"], timeout=10)
                self.assertIsNone(execution._runtime_cleanup)
                self.assertFalse(pool._runtime_executions)
            finally:
                execution._stop_keep_alive()
            # Touching the expired handle fences/quarantines it immediately;
            # otherwise pool close can race the worker's periodic reaper.
            with self.assertRaises(RametError) as expired:
                execution.release()
            self.assertIn(expired.exception.status, (404, 409))


if __name__ == "__main__":
    unittest.main()

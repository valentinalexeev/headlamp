"""API tests with fake providers (no cloud, no provider SDKs). Needs fastapi + httpx; skipped otherwise."""
import json
import os
import sys
import tempfile
import types
import unittest

import _helpers  # noqa: F401  (sys.path)
from app.errors import APIError, NotConfigured

try:
    from fastapi.testclient import TestClient
except Exception:  # noqa: BLE001
    TestClient = None

CONFIG = {
    "title": "t",
    "nodes": {
        "up": {"provider": "yandex", "tailnetName": "u", "yandex": {"instance_id": "SECRET"}},
        "bad": {"provider": "oci"},
        "down": {"provider": "fly", "fly": {"app": "x"}},
    },
}


def fake_provider(behaviour):
    m = types.ModuleType("fake")
    m.calls = []

    def status(node):
        m.calls.append(("status", node))
        return behaviour("status", node)

    def start(node):
        return behaviour("start", node)

    def stop(node):
        return behaviour("stop", node)

    m.status, m.start, m.stop = status, start, stop
    return m


@unittest.skipUnless(TestClient, "fastapi/httpx not installed")
class Api(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(CONFIG, f)
        os.environ["NODES_CONFIG"] = f.name
        sys.modules["app.yandex"] = fake_provider(lambda op, n: {"status": "RUNNING"} if op == "status" else ("RUNNING", "STOPPING"))

        def oci(op, n):
            raise NotConfigured("oci is not set up")

        def fly(op, n):
            raise APIError("boom")

        sys.modules["app.oci"] = fake_provider(oci)
        sys.modules["app.fly"] = fake_provider(fly)
        sys.modules.pop("app.main", None)
        from app import main

        cls.client = TestClient(main.app)

    def test_nodes_json_has_no_connection_details(self):
        body = self.client.get("/nodes.json").json()
        self.assertEqual(set(body["nodes"]), {"up", "bad", "down"})
        self.assertNotIn("SECRET", json.dumps(body))
        self.assertEqual(body["nodes"]["up"]["tailnetName"], "u")

    def test_status_is_per_node_and_soft(self):
        body = self.client.get("/status").json()
        self.assertEqual(body["up"], {"status": "RUNNING"})
        self.assertEqual(body["bad"]["status"], "not_configured")   # does not break the others
        self.assertEqual(body["down"], {"status": "error", "detail": "boom"})

    def test_actions(self):
        r = self.client.post("/nodes/up/stop")
        self.assertEqual(r.json(), {"node": "up", "action": "stop", "previous_status": "RUNNING", "current_status": "STOPPING"})
        self.assertEqual(self.client.post("/nodes/bad/start").status_code, 503)
        self.assertEqual(self.client.post("/nodes/down/start").status_code, 502)

    def test_unknown_node_is_404(self):
        self.assertEqual(self.client.post("/nodes/nope/start").status_code, 404)
        self.assertEqual(self.client.get("/nodes/nope/status").status_code, 404)

    def test_routes_are_only_under_nodes(self):
        self.assertEqual(self.client.post("/up/start").status_code, 404)

    def test_healthz_and_page(self):
        self.assertEqual(self.client.get("/healthz").json(), {"status": "ok"})
        self.assertIn("page-title", self.client.get("/").text)


if __name__ == "__main__":
    unittest.main()

"""Enrolment admin API tests: real Store on a temp DB, fake agent (no network)."""
import hashlib
import os
import tempfile
import time
import unittest
from unittest import mock

import _helpers  # noqa: F401  (sys.path)
from app.enroll.agent_client import AgentConflict, AgentRejected, AgentUnauthorized, AgentUnavailable
from app.enroll.profile import IngressCfg
from app.enroll.store import Store

try:
    from fastapi import FastAPI
    from fastapi.testclient import TestClient
except Exception:  # noqa: BLE001
    TestClient = None

CFG = IngressCfg(host="ingress.example.test", public_key="PUB", short_id="ab", sni="sni.example.test")
NOW = 1_800_000_000
FUTURE = NOW + 86400 * 30


class FakeAgent:
    def __init__(self):
        self.up = True
        self.pushes = []  # (creds, confirm_empty)
        self.stats = {}
        self.fail = None  # exception instance to raise from put_credentials

    def put_credentials(self, creds, confirm_empty=False):
        if self.fail is not None:
            raise self.fail
        if not self.up:
            raise AgentUnavailable("ConnectionError")
        self.pushes.append((creds, confirm_empty))

    def get_stats(self):
        return self.stats if self.up else {}


@unittest.skipUnless(TestClient, "fastapi/httpx not installed")
class Admin(unittest.TestCase):
    def setUp(self):
        from app.enroll import admin

        self.admin = admin
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(os.path.join(self.tmp.name, "e.db"), b"k" * 32, clock=lambda: NOW)
        self.agent = FakeAgent()
        app = FastAPI()
        self.router = admin.create_router(self.store, CFG, self.agent)
        app.include_router(self.router)
        self.c = TestClient(app)

    def tearDown(self):
        self.tmp.cleanup()

    def create(self, **over):
        body = {"name": "Anna phone", "owner": "anna", "client": "sing-box", "expires_at": FUTURE}
        body.update(over)
        return self.c.post("/enroll/devices", json=body)

    def test_create_pushes_three_credentials(self):
        r = self.create()
        self.assertEqual(r.status_code, 200)
        body = r.json()
        self.assertFalse(body["sync_pending"])
        self.assertTrue(body["subscription_url"].startswith("https://ingress.example.test/sub/"))
        creds, confirm = self.agent.pushes[-1]
        self.assertEqual(len(creds), 3)
        self.assertFalse(confirm)
        dev = self.store.list_devices()[0]
        self.assertEqual({c["uuid"] for c in creds}, set(dev.uuids.values()))

    def test_revoke_removes_credentials_and_confirms_empty(self):
        dev_id = self.create().json()["id"]
        uuids = set(self.store.list_devices()[0].uuids.values())
        r = self.c.post(f"/enroll/devices/{dev_id}/revoke", json={})
        self.assertEqual(r.status_code, 200)
        creds, confirm = self.agent.pushes[-1]
        self.assertFalse({c["uuid"] for c in creds} & uuids)
        self.assertTrue(confirm)  # store has devices, none live: deliberate

    def test_empty_store_is_not_pushed_and_not_alarming(self):
        # the agent answers 409 to an unconfirmed empty push, and an empty store must never wipe
        # it, so there is nothing to push: no request, no pending state
        state = self.admin.SyncState(clock=lambda: NOW)
        self.assertTrue(self.admin.sync(self.store, self.agent, state))
        self.assertEqual(self.agent.pushes, [])
        self.assertEqual(state.snapshot(), {"pending": False, "state": "empty", "since": None})

    def test_each_failed_sync_is_classified_and_logged(self):
        self.create()
        uuids = list(self.store.list_devices()[0].uuids.values())
        cases = (
            (AgentUnavailable("ConnectionError"), "unreachable", "AgentUnavailable", None),
            (AgentUnauthorized("agent answered 401", status=401), "unauthorized", "AgentUnauthorized", "401"),
            (AgentRejected("agent answered 503", status=503), "rejected", "AgentRejected", "503"),
            (AgentConflict("agent answered 409", status=409), "conflict", "AgentConflict", "409"),
        )
        for exc, outcome, cls, status in cases:
            state = self.admin.SyncState(clock=lambda: NOW)
            self.agent.fail = exc
            with self.assertLogs("app.enroll.admin", level="WARNING") as logs:
                self.assertFalse(self.admin.sync(self.store, self.agent, state))
            out = "".join(logs.output)
            self.assertIn(outcome, out)
            self.assertIn(cls, out)
            if status:
                self.assertIn(status, out)
            for u in uuids:
                self.assertNotIn(u, out)
            self.assertEqual(state.snapshot(), {"pending": True, "state": outcome, "since": NOW})

    def test_since_is_kept_while_failing_and_cleared_on_success(self):
        self.create()
        t = [NOW]
        state = self.admin.SyncState(clock=lambda: t[0])
        self.agent.up = False
        with self.assertLogs("app.enroll.admin", level="WARNING"):
            self.admin.sync(self.store, self.agent, state)
            t[0] += 300
            self.agent.fail = AgentRejected("agent answered 503", status=503)
            self.admin.sync(self.store, self.agent, state)
        self.assertEqual(state.snapshot(), {"pending": True, "state": "rejected", "since": NOW})
        self.agent.fail, self.agent.up = None, True
        self.assertTrue(self.admin.sync(self.store, self.agent, state))
        self.assertEqual(state.snapshot(), {"pending": False, "state": "ok", "since": None})

    def test_list_reports_sync_state_for_the_banner(self):
        r = self.c.get("/enroll/devices").json()
        self.assertEqual(r["devices"], [])
        self.assertEqual(r["sync"], {"pending": False, "state": "unknown", "since": None})
        dev_id = self.create().json()["id"]
        self.agent.fail = AgentRejected("agent answered 503", status=503)
        with self.assertLogs("app.enroll.admin", level="WARNING"):
            body = self.c.post(f"/enroll/devices/{dev_id}/revoke", json={}).json()
        self.assertTrue(body["sync_pending"])
        self.assertEqual(body["sync"], {"pending": True, "state": "rejected", "since": NOW})
        r = self.c.get("/enroll/devices").json()
        self.assertEqual(r["sync"], {"pending": True, "state": "rejected", "since": NOW})
        self.assertTrue(r["devices"][0]["revoked"])

    def test_extend_reports_sync_state(self):
        dev_id = self.create().json()["id"]
        self.agent.fail = AgentUnauthorized("agent answered 401", status=401)
        with self.assertLogs("app.enroll.admin", level="WARNING"):
            body = self.c.post(f"/enroll/devices/{dev_id}/extend", json={"expires_at": FUTURE + 10}).json()
        self.assertEqual(body["sync"], {"pending": True, "state": "unauthorized", "since": NOW})
        self.assertTrue(body["sync_pending"])
        self.agent.fail = None
        body = self.c.post(f"/enroll/devices/{dev_id}/extend", json={"expires_at": FUTURE + 20}).json()
        self.assertEqual(body["sync"], {"pending": False, "state": "ok", "since": None})
        self.assertFalse(body["sync_pending"])

    def test_agent_down_on_create_then_reconcile(self):
        self.agent.up = False
        with self.assertLogs("app.enroll.admin", level="WARNING"):
            r = self.create()
        self.assertEqual(r.status_code, 200)
        self.assertTrue(r.json()["sync_pending"])
        self.assertEqual(len(self.store.list_devices()), 1)
        self.assertEqual(self.agent.pushes, [])
        self.agent.up = True
        state = self.router.sync_state
        self.assertTrue(state.pending)
        self.assertTrue(self.admin.reconcile(self.store, self.agent, state))
        self.assertFalse(state.pending)
        self.assertEqual(len(self.agent.pushes[-1][0]), 3)

    def test_extend_moves_not_after(self):
        dev_id = self.create().json()["id"]
        new = FUTURE + 1000
        r = self.c.post(f"/enroll/devices/{dev_id}/extend", json={"expires_at": new})
        self.assertEqual(r.status_code, 200)
        self.assertEqual({c["not_after"] for c in self.agent.pushes[-1][0]}, {new})

    def test_link_url(self):
        dev_id = self.create().json()["id"]
        r = self.c.post(f"/enroll/devices/{dev_id}/links", json={"ttl": 3600, "max_uses": 2})
        url = r.json()["url"]
        self.assertTrue(url.startswith("https://ingress.example.test/e/"))
        self.assertIsNotNone(self.store.redeem_token(url.rsplit("/", 1)[1]))

    def test_rotate_secret(self):
        dev_id = self.create().json()["id"]
        old = self.store.subscription_secret(dev_id)
        r = self.c.post(f"/enroll/devices/{dev_id}/rotate-secret", json={})
        self.assertNotIn(old, r.json()["subscription_url"])
        self.assertIn(self.store.subscription_secret(dev_id), r.json()["subscription_url"])

    def test_list_leaks_nothing_and_has_traffic(self):
        dev_id = self.create().json()["id"]
        link = self.c.post(f"/enroll/devices/{dev_id}/links", json={"ttl": 3600, "max_uses": 1}).json()["url"]
        dev = self.store.list_devices()[0]
        opaque = hashlib.sha256(dev.uuids["auto"].encode()).hexdigest()[:8]
        self.agent.stats = {opaque: {"up": 5, "down": 7}}
        r = self.c.get("/enroll/devices")
        self.assertEqual(r.status_code, 200)
        raw = r.text
        for u in dev.uuids.values():
            self.assertNotIn(u, raw)
        self.assertNotIn(self.store.subscription_secret(dev_id), raw)
        self.assertNotIn(self.store.subscription_secret(dev_id).split(".")[1], raw)
        self.assertNotIn(link.rsplit("/", 1)[1], raw)
        self.assertNotIn("token", raw.lower())
        row = r.json()["devices"][0]
        self.assertEqual(row["traffic"], {"up": 5, "down": 7})
        self.assertEqual(
            {k: row[k] for k in ("id", "name", "owner", "client", "expires_at", "revoked")},
            {"id": dev_id, "name": "Anna phone", "owner": "anna", "client": "sing-box",
             "expires_at": FUTURE, "revoked": False},
        )

    def test_list_without_agent_stats(self):
        self.create()
        self.agent.up = False
        row = self.c.get("/enroll/devices").json()["devices"][0]
        self.assertIsNone(row["traffic"])

    def test_validation_422(self):
        self.assertEqual(self.create(client="wireguard").status_code, 422)
        self.assertEqual(self.create(expires_at=NOW - 1).status_code, 422)
        self.assertEqual(self.create(expires_at="soon").status_code, 422)
        self.assertEqual(self.create(name="").status_code, 422)
        dev_id = self.create().json()["id"]
        for body in ({"ttl": 59, "max_uses": 1}, {"ttl": 604801, "max_uses": 1},
                     {"ttl": 3600, "max_uses": 0}, {"ttl": 3600, "max_uses": 101}):
            r = self.c.post(f"/enroll/devices/{dev_id}/links", json=body)
            self.assertEqual(r.status_code, 422, body)
        r = self.c.post(f"/enroll/devices/{dev_id}/extend", json={"expires_at": NOW - 5})
        self.assertEqual(r.status_code, 422)

    def test_unknown_device_404(self):
        for path, body in (("links", {"ttl": 3600, "max_uses": 1}), ("revoke", {}),
                           ("extend", {"expires_at": FUTURE}), ("rotate-secret", {})):
            r = self.c.post(f"/enroll/devices/999/{path}", json=body)
            self.assertEqual(r.status_code, 404, path)

    def test_non_json_content_type_rejected(self):
        r = self.c.post("/enroll/devices", content="name=x&owner=y&client=sing-box&expires_at=1",
                        headers={"Content-Type": "application/x-www-form-urlencoded"})
        self.assertEqual(r.status_code, 422)
        dev_id = self.create().json()["id"]
        r = self.c.post(f"/enroll/devices/{dev_id}/revoke", content="",
                        headers={"Content-Type": "text/plain"})
        self.assertEqual(r.status_code, 422)
        self.assertFalse(self.store.list_devices()[0].revoked)

    def test_page(self):
        app = FastAPI()
        app.include_router(self.admin.create_router(self.store, CFG, self.agent))
        r = TestClient(app).get("/enroll")
        self.assertEqual(r.status_code, 200)
        self.assertIn("text/html", r.headers["content-type"])
        self.assertIn("<form", r.text)
        self.assertIn('id="syncbanner"', r.text)
        self.assertIn("ingress host out of sync since", r.text)


@unittest.skipUnless(TestClient, "fastapi/httpx not installed")
class Wiring(unittest.TestCase):
    def test_no_enroll_routes_when_env_unset(self):
        import sys

        saved = sys.modules.pop("app.main", None)
        try:
            with mock.patch.dict(os.environ):
                os.environ.pop("ENROLL_DB", None)
                from app import main

                c = TestClient(main.app)
                self.assertEqual(c.get("/enroll").status_code, 404)
                self.assertEqual(c.get("/enroll/devices").status_code, 404)
        finally:
            sys.modules.pop("app.main", None)
            if saved is not None:
                sys.modules["app.main"] = saved


class Sync(unittest.TestCase):
    def test_concurrent_syncs_end_with_newest_state(self):
        import threading

        from app.enroll import admin

        with tempfile.TemporaryDirectory() as tmp:
            store = Store(os.path.join(tmp, "e.db"), b"k" * 32, clock=lambda: NOW)
            dev, _ = store.create_device("n", "o", "sing-box", FUTURE)
            started, release = threading.Event(), threading.Event()
            pushes = []

            class Slow:
                def put_credentials(self, creds, confirm_empty=False):
                    first = not started.is_set()
                    if first:
                        started.set()
                        release.wait(5)
                    pushes.append(creds)  # recorded on completion = order of arrival

            agent, state = Slow(), admin.SyncState()
            t1 = threading.Thread(target=admin.sync, args=(store, agent, state))
            t1.start()
            self.assertTrue(started.wait(5))
            store.revoke_device(dev.id)
            t2 = threading.Thread(target=admin.sync, args=(store, agent, state))
            t2.start()
            time.sleep(0.2)  # let t2 run ahead if nothing serialises it
            release.set()
            t1.join(5)
            t2.join(5)
            self.assertEqual(len(pushes[0]), 3)
            self.assertEqual(pushes[-1], [])


ENV_KEYS = ("ENROLL_DB", "ENROLL_CONFIG", "ENROLL_SECRET_KEY", "AGENT_URL", "AGENT_TOKEN", "ENROLL_ALLOWED_HOSTS")


@unittest.skipUnless(TestClient, "fastapi/httpx not installed")
class AttachFromEnv(unittest.TestCase):
    def setUp(self):
        import json

        self.tmp = tempfile.TemporaryDirectory()
        cfgp = os.path.join(self.tmp.name, "c.json")
        with open(cfgp, "w") as f:
            json.dump({"host": CFG.host, "public_key": "P", "short_id": "ab", "sni": "s"}, f)
        self.env = {
            "ENROLL_DB": os.path.join(self.tmp.name, "e.db"), "ENROLL_CONFIG": cfgp,
            "ENROLL_SECRET_KEY": "k" * 32, "AGENT_URL": "http://agent.test", "AGENT_TOKEN": "tok",
        }

    def tearDown(self):
        self.tmp.cleanup()

    def attach(self, env):
        from app.enroll import admin

        app = FastAPI()
        with mock.patch.dict(os.environ, env, clear=False):
            for k in ENV_KEYS:
                if k not in env:
                    os.environ.pop(k, None)
            with mock.patch.object(admin, "AgentClient") as ac:
                return app, admin.attach_from_env(app), ac

    def test_builds_router_from_env(self):
        app, attached, ac = self.attach(self.env)
        self.assertTrue(attached)
        ac.assert_called_once_with("http://agent.test", "tok")
        self.assertEqual(TestClient(app).get("/enroll/devices").status_code, 200)

    def test_allowed_hosts(self):
        env = dict(self.env, ENROLL_ALLOWED_HOSTS="admin.example.test, 127.0.0.1")
        app, _, _ = self.attach(env)
        self.assertEqual(TestClient(app, base_url="http://admin.example.test").get("/enroll/devices").status_code, 200)
        self.assertEqual(TestClient(app, base_url="http://127.0.0.1").get("/enroll/devices").status_code, 200)
        self.assertEqual(TestClient(app, base_url="http://evil.example.test").get("/enroll/devices").status_code, 400)
        self.assertEqual(TestClient(app, base_url="http://evil.example.test").post(
            "/enroll/devices", json={}).status_code, 400)

    def test_no_host_check_without_allowed_hosts(self):
        app, _, _ = self.attach(self.env)
        self.assertEqual(TestClient(app, base_url="http://anything.example.test").get("/enroll/devices").status_code, 200)

    def test_disabled_without_db(self):
        env = dict(self.env)
        del env["ENROLL_DB"]
        app, attached, _ = self.attach(env)
        self.assertFalse(attached)

    def test_missing_vars_give_clear_error(self):
        for k in ("ENROLL_SECRET_KEY", "ENROLL_CONFIG", "AGENT_URL", "AGENT_TOKEN"):
            env = dict(self.env)
            del env[k]
            with self.assertRaises(RuntimeError) as cm:
                self.attach(env)
            self.assertIn(k, str(cm.exception))

    def test_lifespan_starts_and_cancels_loop(self):
        import asyncio

        from app.enroll import admin

        with mock.patch.object(admin, "RECONCILE_SECONDS", 0.01), \
                mock.patch.object(admin, "reconcile") as rec:
            app, _, _ = self.attach(self.env)
            with TestClient(app):  # runs startup, then shutdown
                import time

                time.sleep(0.2)
            self.assertGreaterEqual(rec.call_count, 1)
            n = rec.call_count
            time.sleep(0.1)
            self.assertEqual(rec.call_count, n)  # cancelled

    def test_loop_survives_exception_and_marks_pending(self):
        import asyncio

        from app.enroll import admin

        calls = []

        def boom(store, agent, state):
            calls.append(1)
            if len(calls) == 1:
                raise RuntimeError("db locked SECRETPAYLOAD")

        state = admin.SyncState()

        async def run():
            with mock.patch.object(admin, "RECONCILE_SECONDS", 0.01), \
                    mock.patch.object(admin, "reconcile", boom), \
                    self.assertLogs("app.enroll.admin", level="ERROR") as logs:
                task = asyncio.create_task(admin.reconcile_loop(None, None, state))
                await asyncio.sleep(0.2)
                task.cancel()
            return logs.output

        out = asyncio.run(run())
        self.assertGreaterEqual(len(calls), 2)
        self.assertIn("RuntimeError", "".join(out))
        self.assertNotIn("SECRETPAYLOAD", "".join(out))
        self.assertTrue(state.pending)
        self.assertEqual(state.state, "error")


class AgentClientTests(unittest.TestCase):
    def test_put_credentials_request_shape(self):
        from unittest import mock

        from app.enroll import agent_client

        resp = mock.Mock(status_code=204)
        with mock.patch.object(agent_client.requests, "put", return_value=resp) as put:
            agent_client.AgentClient("http://a:1/", "tok").put_credentials([{"uuid": "u"}])
            args, kw = put.call_args
            self.assertEqual(args[0], "http://a:1/v1/credentials")
            self.assertEqual(kw["headers"]["Authorization"], "Bearer tok")
            self.assertEqual(kw["json"], {"credentials": [{"uuid": "u"}]})
            self.assertFalse(kw.get("params"))
            self.assertEqual(kw["timeout"], 10)
            agent_client.AgentClient("http://a:1", "tok").put_credentials([], confirm_empty=True)
            self.assertEqual(put.call_args[1]["params"], {"confirm_empty": "true"})

    def test_status_codes_become_typed_errors(self):
        from unittest import mock

        import requests

        from app.enroll import agent_client

        c = agent_client.AgentClient("http://a:1", "t")
        for code, cls, outcome in ((401, AgentUnauthorized, "unauthorized"), (403, AgentUnauthorized, "unauthorized"),
                                   (409, AgentConflict, "conflict"), (503, AgentRejected, "rejected"),
                                   (500, AgentRejected, "rejected"), (422, AgentRejected, "rejected")):
            with mock.patch.object(agent_client.requests, "put", return_value=mock.Mock(status_code=code)):
                with self.assertRaises(cls) as cm:
                    c.put_credentials([{"uuid": "u"}])
            self.assertIsInstance(cm.exception, AgentUnavailable)
            self.assertEqual(cm.exception.status, code)
            self.assertEqual(cm.exception.outcome, outcome)
        with mock.patch.object(agent_client.requests, "put", side_effect=requests.ConnectionError("SECRET")):
            with self.assertRaises(AgentUnavailable) as cm:
                c.put_credentials([])
        self.assertIs(type(cm.exception), AgentUnavailable)
        self.assertEqual(cm.exception.outcome, "unreachable")
        self.assertIsNone(cm.exception.status)
        self.assertEqual(str(cm.exception), "ConnectionError")

    def test_stats_timeout_is_short(self):
        from unittest import mock

        from app.enroll import agent_client

        resp = mock.Mock(status_code=200)
        resp.json.return_value = {"abcd1234": {"up": 1, "down": 2}}
        with mock.patch.object(agent_client.requests, "get", return_value=resp) as get:
            self.assertEqual(agent_client.AgentClient("http://a:1", "t").get_stats(), {"abcd1234": {"up": 1, "down": 2}})
        self.assertEqual(get.call_args[1]["timeout"], 2)

    def test_errors_become_agent_unavailable(self):
        from unittest import mock

        import requests

        from app.enroll import agent_client

        c = agent_client.AgentClient("http://a:1", "t")
        with mock.patch.object(agent_client.requests, "put", side_effect=requests.ConnectionError()):
            with self.assertRaises(AgentUnavailable):
                c.put_credentials([])
        with mock.patch.object(agent_client.requests, "put", return_value=mock.Mock(status_code=500)):
            with self.assertRaises(AgentUnavailable):
                c.put_credentials([])
        with mock.patch.object(agent_client.requests, "get", side_effect=requests.Timeout()):
            self.assertEqual(c.get_stats(), {})


if __name__ == "__main__":
    unittest.main()

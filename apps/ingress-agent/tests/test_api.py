import hashlib
import json
import logging
import os
import unittest

from fastapi.testclient import TestClient

from _helpers import NOW, TOKEN, U1, U2, U3, Env, body, cred
from app.main import create_app, create_app_from_env, sweep_loop
from app.store import load
from app.xray import email_for

AUTH = {"Authorization": f"Bearer {TOKEN}"}


def opaque(u):
    return hashlib.sha256(u.encode()).hexdigest()[:8]


class ApiTests(unittest.TestCase):
    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.cleanup)
        self.client = TestClient(create_app(self.env.applier, TOKEN, lambda: self.env.now))

    def put(self, payload, headers=AUTH, **params):
        return self.client.put("/v1/credentials", json=payload, headers=headers, params=params)

    def test_auth(self):
        for h in ({}, {"Authorization": "Bearer nope"}, {"Authorization": TOKEN}):
            r = self.put(body(cred(U1)), headers=h)
            self.assertEqual(r.status_code, 401)
            self.assertEqual(r.content, b"")
        self.assertEqual(self.client.get("/v1/stats").status_code, 401)
        self.assertEqual(self.put({"junk": 1}, headers={}).status_code, 401)
        self.assertEqual(self.env.proc.calls, [])

    def test_empty_needs_confirmation(self):
        self.assertEqual(self.put({"credentials": []}).status_code, 409)
        self.assertEqual(self.put({"credentials": []}, confirm_empty="true").status_code, 200)

    def test_validation(self):
        bad = [
            {"credentials": [{"uuid": U1, "mode": "mars", "not_after": NOW + 5}]},
            {"credentials": [{"uuid": "not-a-uuid", "mode": "auto", "not_after": NOW + 5}]},
            {"credentials": [{"uuid": U1, "mode": "auto", "not_after": "soon"}]},
            {"credentials": [{"uuid": U1, "mode": "auto", "not_after": 1.5}]},
        ]
        for b in bad:
            self.assertEqual(self.put(b).status_code, 422, b)
        self.assertEqual(self.env.proc.calls, [])

    def test_duplicate_uuid_rejected(self):
        for second in ("auto", "direct"):
            r = self.put(body(cred(U1, "auto"), cred(U1.upper(), second)))
            self.assertEqual(r.status_code, 422)
        self.assertEqual(self.env.proc.calls, [])

    def test_all_expired_needs_confirm_empty(self):
        self.assertEqual(self.put(body(cred(U1, not_after=NOW))).status_code, 409)
        self.assertEqual(self.put(body(cred(U1, not_after=NOW)), confirm_empty="true").status_code, 200)

    def test_restart_failure_then_retry(self):
        self.put(body(cred(U1)))
        saved = self.env.state.read_text()
        self.env.proc.restart_failures = 1
        self.assertEqual(self.put(body(cred(U1), cred(U2))).status_code, 503)
        self.assertEqual(self.env.state.read_text(), saved)
        self.env.proc.calls.clear()
        self.assertEqual(self.put(body(cred(U1), cred(U2))).status_code, 200)
        self.assertEqual(self.env.proc.kinds(), ["test", "restart"])
        self.assertEqual(len(load(self.env.state)), 2)

    def test_changed_push_tests_then_restarts(self):
        r = self.put(body(cred(U1), cred(U2, "direct")))
        self.assertEqual(r.status_code, 200)
        self.assertEqual(self.env.proc.kinds(), ["test", "restart"])
        cfg = json.loads(self.env.config.read_text())
        self.assertEqual(len(cfg["inbounds"][1]["settings"]["clients"]), 2)
        self.assertEqual(len(load(self.env.state)), 2)

    def test_the_validated_temp_file_ends_in_json(self):
        # xray derives the config format from the extension: a temp name without .json fails `xray run -test` on a real host.
        self.assertEqual(self.put(body(cred(U1))).status_code, 200)
        tested = [a for a in self.env.proc.calls if "-test" in a]
        self.assertEqual(len(tested), 1)
        path = tested[0][tested[0].index("-c") + 1]
        self.assertTrue(path.endswith(".json"), path)
        self.assertEqual(os.path.dirname(path), str(self.env.config.parent))

    def test_unchanged_push_does_not_restart(self):
        self.put(body(cred(U1)))
        self.env.proc.calls.clear()
        self.assertEqual(self.put(body(cred(U1))).status_code, 200)
        self.assertEqual(self.env.proc.calls, [])

    def test_expired_dropped_on_put(self):
        self.put(body(cred(U1, not_after=NOW), cred(U2)))
        self.assertEqual([c.uuid for c in load(self.env.state)], [U2])

    def test_invalid_config_keeps_everything(self):
        self.put(body(cred(U1)))
        old = self.env.config.read_text()
        old_state = self.env.state.read_text()
        self.env.proc.test_ok = False
        self.env.proc.calls.clear()
        r = self.put(body(cred(U1), cred(U2)))
        self.assertGreaterEqual(r.status_code, 500)
        self.assertEqual(self.env.config.read_text(), old)
        self.assertEqual(self.env.state.read_text(), old_state)
        self.assertEqual(self.env.proc.kinds(), ["test"])
        self.assertEqual(os.listdir(self.env.config.parent).count("config.json"), 1)
        self.assertEqual(len(os.listdir(self.env.config.parent)), 3)  # base, config, state dir: no temp leftovers

    def test_state_file_mode(self):
        self.put(body(cred(U1)))
        self.assertEqual(os.stat(self.env.state).st_mode & 0o777, 0o600)

    def test_stats_opaque(self):
        self.put(body(cred(U1), cred(U2)))
        self.env.proc.stats_out = json.dumps({"stat": [
            {"name": f"user>>>{email_for(U1)}>>>traffic>>>uplink", "value": "3"},
            {"name": f"user>>>{email_for(U1)}>>>traffic>>>downlink", "value": "4"},
            {"name": "user>>>stranger@i>>>traffic>>>uplink", "value": "9"},
        ]})
        r = self.client.get("/v1/stats", headers=AUTH)
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json(), {opaque(U1): {"up": 3, "down": 4}})
        for u in (U1, U2):
            self.assertNotIn(u, r.text)

    def test_no_other_routes(self):
        for p in ("/docs", "/openapi.json", "/redoc", "/", "/v1/other"):
            self.assertEqual(self.client.get(p, headers=AUTH).status_code, 404, p)

    def test_logs_contain_no_uuids(self):
        with self.assertLogs("ingress_agent", level="DEBUG") as cap:
            self.put(body(cred(U1), cred(U2)))
            self.put(body(cred(U3)))
            self.env.proc.test_ok = False
            self.put(body(cred(U1)))
            logging.getLogger("ingress_agent").info("sentinel")
        text = "\n".join(cap.output)
        for u in (U1, U2, U3):
            self.assertNotIn(u, text)
            self.assertNotIn(email_for(u), text)
        self.assertNotIn(TOKEN, text)
        self.assertIn("applied: +2 -0", text)


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.env = Env()
        self.addCleanup(self.env.cleanup)

    def test_expire_sweeps_and_reapplies(self):
        a = self.env.applier
        a.apply([cred(U1, not_after=NOW + 10), cred(U2)])
        self.env.proc.calls.clear()
        a.expire()  # nothing expired yet
        self.assertEqual(self.env.proc.calls, [])
        self.env.now = NOW + 10
        a.expire()
        self.assertEqual(self.env.proc.kinds(), ["test", "restart"])
        self.assertEqual([c.uuid for c in load(self.env.state)], [U2])

    def test_restore_on_startup(self):
        self.env.applier.apply([cred(U1)])
        old = self.env.config.read_text()
        self.env.config.write_text("{}")  # config lost, state kept
        self.env.proc.calls.clear()
        with TestClient(create_app(self.env.applier, TOKEN, lambda: self.env.now)):
            pass
        self.assertEqual(self.env.config.read_text(), old)
        self.assertEqual(self.env.proc.kinds(), ["test", "restart"])

    def test_restore_identical_does_not_restart(self):
        self.env.applier.apply([cred(U1)])
        self.env.proc.calls.clear()
        with TestClient(create_app(self.env.applier, TOKEN, lambda: self.env.now)):
            pass
        self.assertEqual(self.env.proc.calls, [])

    def test_restore_after_half_applied_restarts_once(self):
        self.env.applier.apply([cred(U1)])
        self.env.proc.restart_failures = 1
        with self.assertRaises(Exception):
            self.env.applier.apply([cred(U1), cred(U2)])  # config replaced, restart failed, state old
        self.env.proc.calls.clear()
        with TestClient(create_app(self.env.applier, TOKEN, lambda: self.env.now)):
            pass
        self.assertEqual(self.env.proc.kinds(), ["test", "restart"])
        self.assertEqual(len(json.loads(self.env.config.read_text())["inbounds"][1]["settings"]["clients"]), 1)

    def test_expire_does_not_revert_concurrent_put(self):
        import threading

        a = self.env.applier
        a.apply([cred(U1, not_after=NOW + 10)])
        self.env.now = NOW + 10
        orig, fired = a.current, []

        def current():
            cur = orig()
            if not fired:
                fired.append(1)
                t = threading.Thread(target=a.apply, args=([cred(U2, not_after=NOW + 500)],))
                t.start()
                self.thread = t
                t.join(0.3)  # old code: PUT finishes here; fixed code: it waits for the lock
            return cur

        a.current = current
        a.expire()
        self.thread.join(5)
        self.assertEqual([c.uuid for c in orig()], [U2])

    def test_sweep_loop_calls_expire(self):
        import asyncio

        calls = []

        class A:
            def expire(self):
                calls.append(1)

        async def fake_sleep(s):
            if len(calls) >= 2:
                raise asyncio.CancelledError
            self.assertEqual(s, 7)

        with self.assertRaises(asyncio.CancelledError):
            asyncio.run(sweep_loop(A(), 7, fake_sleep))
        self.assertEqual(len(calls), 2)

    def test_env_requires_token(self):
        old = os.environ.pop("AGENT_TOKEN", None)
        try:
            with self.assertRaises(RuntimeError) as cm:
                create_app_from_env()
            self.assertIn("AGENT_TOKEN", str(cm.exception))
        finally:
            if old is not None:
                os.environ["AGENT_TOKEN"] = old


if __name__ == "__main__":
    unittest.main()

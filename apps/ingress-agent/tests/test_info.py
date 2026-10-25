import json
import os
import unittest
from unittest import mock

from fastapi.testclient import TestClient

from _helpers import TOKEN, U1, Env, body, cred
from app.main import create_app, create_app_from_env

AUTH = {"Authorization": f"Bearer {TOKEN}"}
FAKE_PRIVATE_KEY = "fake-private-key-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
INFO_ENV = {
    "INGRESS_NAME": "ingress-1",
    "INGRESS_HOST": "relay.example.com",
    "INGRESS_PUBLIC_KEY": "fake-public-key-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
    "INGRESS_SHORT_ID": "0123456789abcdef",
}
ALL_VARS = ("INGRESS_NAME", "INGRESS_HOST", "INGRESS_PORT", "INGRESS_SNI", "INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID")


def make_app(extra_env=None, only=None, token=TOKEN):
    """Builds the app from a controlled environment; returns (client, env)."""
    env = Env()
    base = {"AGENT_TOKEN": token, "AGENT_STATE_DIR": str(env.state.parent), "XRAY_BASE_CONFIG": str(env.base),
            "XRAY_CONFIG": str(env.config)}
    base.update(INFO_ENV if only is None else only)
    base.update(extra_env or {})
    with mock.patch.dict(os.environ, base, clear=True), mock.patch("app.main.XrayRunner", lambda *a, **k: env.runner):
        app = create_app_from_env()
    return env, TestClient(app)


class InfoTestBase(unittest.TestCase):
    def build(self, *a, **kw):
        env, client = make_app(*a, **kw)
        self.addCleanup(env.cleanup)
        return env, client


class TestInfoRequiresToken(InfoTestBase):
    def test_401_empty_body(self):
        _, c = self.build()
        for h in ({}, {"Authorization": "Bearer nope"}, {"Authorization": TOKEN}):
            r = c.get("/v1/info", headers=h)
            self.assertEqual(r.status_code, 401)
            self.assertEqual(r.content, b"")

    def test_401_even_when_unconfigured(self):
        _, c = self.build(only={})
        r = c.get("/v1/info")
        self.assertEqual((r.status_code, r.content), (401, b""))


class TestInfoReturnsConfiguredValues(InfoTestBase):
    def test_defaults_for_port_and_sni(self):
        _, c = self.build()
        r = c.get("/v1/info", headers=AUTH)
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json(), {"name": "ingress-1", "host": "relay.example.com", "port": 443, "sni": "relay.example.com",
                                    "public_key": INFO_ENV["INGRESS_PUBLIC_KEY"], "short_id": "0123456789abcdef"})
        self.assertIsInstance(r.json()["port"], int)

    def test_explicit_port_and_sni(self):
        _, c = self.build({"INGRESS_PORT": "8443", "INGRESS_SNI": "front.example.com"})
        j = c.get("/v1/info", headers=AUTH).json()
        self.assertEqual((j["port"], j["sni"], j["host"]), (8443, "front.example.com", "relay.example.com"))

    def test_empty_port_and_sni_use_defaults(self):
        _, c = self.build({"INGRESS_PORT": "", "INGRESS_SNI": ""})
        j = c.get("/v1/info", headers=AUTH).json()
        self.assertEqual((j["port"], j["sni"]), (443, "relay.example.com"))

    def test_exact_keys(self):
        _, c = self.build()
        self.assertEqual(set(c.get("/v1/info", headers=AUTH).json()), {"name", "host", "port", "sni", "public_key", "short_id"})


class TestInfoUnconfigured503(InfoTestBase):
    def test_each_required_missing_or_empty(self):
        for var in ("INGRESS_NAME", "INGRESS_HOST", "INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID"):
            for how in ("unset", "empty"):
                only = dict(INFO_ENV)
                if how == "unset":
                    del only[var]
                else:
                    only[var] = ""
                _, c = self.build(only=only)
                r = c.get("/v1/info", headers=AUTH)
                self.assertEqual(r.status_code, 503, (var, how))
                self.assertEqual(r.content, b"", (var, how))

    def test_nothing_set_and_other_routes_still_work(self):
        env, c = self.build(only={})
        self.assertEqual(c.get("/v1/info", headers=AUTH).status_code, 503)
        self.assertEqual(c.get("/v1/stats", headers=AUTH).status_code, 200)


class TestInfoNeverContainsSecrets(InfoTestBase):
    def test_no_token_private_key_or_uuid(self):
        # A private key text is in the process environment under likely names; it must never be read.
        extra = {"REALITY_PRIVATE_KEY": FAKE_PRIVATE_KEY, "INGRESS_PRIVATE_KEY": FAKE_PRIVATE_KEY}
        env, c = self.build(extra)
        r = c.put("/v1/credentials", json=body(cred(U1, not_after=4_000_000_000)), headers=AUTH)
        self.assertEqual(r.status_code, 200)
        with mock.patch.dict(os.environ, extra):  # still in the environment while requests are served
            r = c.get("/v1/info", headers=AUTH)
        self.assertEqual(r.status_code, 200)
        raw = r.text + json.dumps(dict(r.headers))
        for secret in (TOKEN, FAKE_PRIVATE_KEY, U1, U1.replace("-", "")):
            self.assertNotIn(secret, raw)

    def test_the_agent_has_no_private_key_variable(self):
        import app.info as info
        self.assertFalse([n for n in dir(info) if "PRIVATE" in n.upper()])
        src = open(info.__file__).read() + open(os.path.join(os.path.dirname(info.__file__), "main.py")).read()
        self.assertNotIn("PRIVATE_KEY", src)


class TestInfoValidationAtStartup(unittest.TestCase):
    CASES = {
        "INGRESS_NAME": ["Ingress-1", "-a", "a-", "a" * 33, "a b", "a_b"],
        "INGRESS_HOST": ["https://relay.example.com", "relay.example.com:443", "relay.example.com/x", "Relay.Example.com",
                         "relay..example.com", "-relay.example.com", ("a" * 64) + ".example.com", ("a." * 130) + "com",
                         "re lay.example.com", "relay_x.example.com"],
        "INGRESS_SNI": ["https://front.example.com", "front.example.com:1", "FRONT.example.com", "a..b"],
        "INGRESS_PORT": ["0", "65536", "-1", "44.3", "http", "443 ", "+443"],
        "INGRESS_PUBLIC_KEY": ["has space", "tab\tkey", "new\nline"],
        "INGRESS_SHORT_ID": ["ab cd", "ab\ncd"],
    }

    def test_each_invalid_value_refuses_to_start_naming_only_the_variable(self):
        for var, values in self.CASES.items():
            for value in values:
                env = dict(INFO_ENV)
                env[var] = value
                env["AGENT_TOKEN"] = TOKEN
                with mock.patch.dict(os.environ, env, clear=True):
                    with self.assertRaises(RuntimeError, msg=(var, value)) as cm:
                        create_app_from_env()
                msg = str(cm.exception)
                self.assertIn(var, msg)
                if len(value.strip()) > 3:
                    self.assertNotIn(value.strip(), msg)
                self.assertNotIn(TOKEN, msg)

    def test_valid_edge_values_start(self):
        for extra in ({"INGRESS_NAME": "a"}, {"INGRESS_NAME": "a" * 32}, {"INGRESS_NAME": "a-b"}, {"INGRESS_PORT": "1"},
                      {"INGRESS_PORT": "65535"}, {"INGRESS_HOST": "x"}, {"INGRESS_SNI": "front.example.com"}):
            env = dict(INFO_ENV, AGENT_TOKEN=TOKEN, **extra)
            with mock.patch.dict(os.environ, env, clear=True):
                create_app_from_env()

    def test_unset_everything_starts(self):
        with mock.patch.dict(os.environ, {"AGENT_TOKEN": TOKEN}, clear=True):
            create_app_from_env()

    def test_invalid_optional_without_required_still_refuses(self):
        with mock.patch.dict(os.environ, {"AGENT_TOKEN": TOKEN, "INGRESS_PORT": "0"}, clear=True):
            with self.assertRaises(RuntimeError):
                create_app_from_env()


class TestOtherRoutesUnchanged(unittest.TestCase):
    def test_create_app_signature_still_works_without_info(self):
        env = Env()
        self.addCleanup(env.cleanup)
        c = TestClient(create_app(env.applier, TOKEN, lambda: env.now))
        self.assertEqual(c.get("/v1/info", headers=AUTH).status_code, 503)
        self.assertEqual(c.get("/v1/stats", headers=AUTH).status_code, 200)
        self.assertEqual(c.get("/openapi.json").status_code, 404)
        self.assertEqual(c.put("/v1/credentials", json=body(cred(U1, not_after=4_000_000_000)), headers=AUTH).status_code, 200)


if __name__ == "__main__":
    unittest.main()

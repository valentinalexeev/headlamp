"""Public enrolment listener tests. Needs fastapi + httpx; skipped otherwise."""
import os
import tempfile
import unittest

import _helpers  # noqa: F401  (sys.path)
from app.enroll.profile import IngressCfg, deep_link, sub_url
from app.enroll.store import Store

try:
    from fastapi.testclient import TestClient
except Exception:  # noqa: BLE001
    TestClient = None

CFG = IngressCfg(host="ingress.example.com", public_key="PUBKEY", short_id="ab12", sni="www.example.com")


@unittest.skipIf(TestClient is None, "fastapi/httpx not installed")
class PublicListenerTest(unittest.TestCase):
    def setUp(self):
        from app.enroll import public

        self.now = 1_000_000.0
        self.tmp = tempfile.TemporaryDirectory()
        self.store = Store(os.path.join(self.tmp.name, "e.db"), b"test-key-0123456789abcdef", clock=lambda: self.now)
        self.device, self.secret = self.store.create_device(
            "phone", "alice", "sing-box", int(self.now) + 10 * 86400
        )
        app = public.create_app(self.store, CFG)
        self.client = TestClient(app, follow_redirects=False)

    def tearDown(self):
        self.tmp.cleanup()

    @staticmethod
    def _target(r):
        """The deep link a client is sent to: the Location of a 302, or the button's href of the landing page."""
        import html
        import re

        if r.status_code == 302:
            return r.headers["location"]
        return html.unescape(re.search(r'href="([^"]+)"', r.text).group(1))

    def test_karing_gets_a_landing_page_with_a_button(self):
        dev, secret = self.store.create_device("Anna", "anna.owner", "karing", int(self.now) + 86400)
        token = self.store.create_token(dev.id)
        r = self.client.get(f"/e/{token}")
        self.assertEqual(r.status_code, 200)
        self.assertTrue(r.headers["content-type"].startswith("text/html"))
        self.assertEqual(r.headers["cache-control"], "no-store")
        self.assertEqual(self._target(r), deep_link("karing", sub_url(CFG, secret), "tunnel"))
        self.assertIn("Open in Karing", r.text)
        self.assertNotIn("<script", r.text)
        self.assertEqual(self.client.get(f"/e/{token}").status_code, 404)

    def test_clash_gets_a_landing_page_and_a_yaml_profile(self):
        dev, secret = self.store.create_device("Anna", "anna.owner", "clash", int(self.now) + 86400)
        r = self.client.get(f"/e/{self.store.create_token(dev.id)}")
        self.assertEqual(r.status_code, 200)
        self.assertIn("Open in Clash Mi", r.text)
        self.assertEqual(self._target(r), deep_link("clash", sub_url(CFG, secret), "tunnel"))
        p = self.client.get(f"/sub/{secret}")
        self.assertEqual(p.status_code, 200)
        self.assertTrue(p.headers["content-type"].startswith("text/yaml"))
        import json

        self.assertEqual([x["name"] for x in json.loads(p.text)["proxies"]], ["auto", "direct", "exit"])
        self.assertNotIn("Anna", p.text)

    def test_valid_token_redirects_once(self):
        token = self.store.create_token(self.device.id)
        r = self.client.get(f"/e/{token}")
        self.assertEqual(r.status_code, 302)
        self.assertEqual(
            r.headers["location"], deep_link("sing-box", sub_url(CFG, self.secret), "tunnel")
        )
        self.assertEqual(self.client.get(f"/e/{token}").status_code, 404)

    def test_device_name_and_owner_never_reach_the_ingress_host(self):
        from app.enroll import public

        for client in ("sing-box", "karing", "clash"):
            dev, secret = self.store.create_device("Anna Secret Phone", "anna.owner", client, int(self.now) + 86400)
            token = self.store.create_token(dev.id)
            loc = self._target(self.client.get(f"/e/{token}"))
            prof = self.client.get(f"/sub/{secret}").text
            for leaked in ("Anna", "Secret", "anna.owner", "anna"):
                self.assertNotIn(leaked, loc, client)
                self.assertNotIn(leaked, prof, client)
            self.assertTrue(loc.endswith("#tunnel") or loc.endswith("&name=tunnel"), loc)
        # the label is configurable, still never the device name
        cfg = IngressCfg(host=CFG.host, public_key="K", short_id="ab", sni="s.example.com", profile_name="work vpn")
        c = TestClient(public.create_app(self.store, cfg), follow_redirects=False)
        loc = c.get(f"/e/{self.store.create_token(self.device.id)}").headers["location"]
        self.assertTrue(loc.endswith("#work%20vpn"), loc)

    PREVIEW_UAS = (
        "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
        "Twitterbot/1.0",
        "TelegramBot (like TwitterBot)",
        "WhatsApp/2.23.20.0 A",
        "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
        "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)",
        "Mozilla/5.0 (Windows NT 6.1; WOW64) SkypeUriPreview Preview/0.5",
        "LinkedInBot/1.0 (compatible; Mozilla/5.0; Apache-HttpClient +http://www.linkedin.com)",
        "Mozilla/5.0 (compatible; Embedly/0.2; +http://support.embed.ly/)",
        "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
        "Mozilla/5.0 (compatible; SomeCrawler/1.0)",
        "Mozilla/5.0 (compatible; Yeti-Spider)",
        "Snap URL Preview Service; bot; snapchat",
        "Google-PageRenderer Google (+https://developers.google.com/+/web/snippet/)",
        "Mozilla/5.0 something LinkExpanding",
        "FACEBOOKEXTERNALHIT/1.1",
    )
    SAFARI_IOS = ("Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 "
                  "(KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1")
    REAL_CLIENTS = (
        SAFARI_IOS,
        "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
        "SFI/1.11.4 (Build 1; sing-box 1.11.4; language en_US)",
        "karing/1.1.2.606 (iOS 17.5; iPhone15,2)",
        "sing-box 1.11.4",
    )

    def test_link_previews_get_plain_404_and_do_not_burn_the_token(self):
        token = self.store.create_token(self.device.id)  # max_uses=1
        baseline = self.client.get("/no/such/path")
        for ua in self.PREVIEW_UAS:
            r = self.client.get(f"/e/{token}", headers={"User-Agent": ua})
            self.assertEqual(r.status_code, 404, ua)
            self.assertEqual(r.content, baseline.content, ua)
            self.assertEqual(r.headers, baseline.headers, ua)
        r = self.client.get(f"/e/{token}", headers={"User-Agent": self.SAFARI_IOS})
        self.assertEqual(r.status_code, 302)
        self.assertEqual(self.client.get(f"/e/{token}", headers={"User-Agent": self.SAFARI_IOS}).status_code, 404)

    def test_real_clients_are_not_treated_as_previews(self):
        for ua in self.REAL_CLIENTS:
            token = self.store.create_token(self.device.id)
            r = self.client.get(f"/e/{token}", headers={"User-Agent": ua})
            self.assertEqual(r.status_code, 302, ua)

    def test_empty_or_missing_user_agent_is_a_normal_client(self):
        token = self.store.create_token(self.device.id)
        self.assertEqual(self.client.get(f"/e/{token}", headers={"User-Agent": ""}).status_code, 302)
        token = self.store.create_token(self.device.id)
        self.client.headers.pop("user-agent", None)
        self.assertNotIn("user-agent", self.client.headers)
        self.assertEqual(self.client.get(f"/e/{token}").status_code, 302)

    def test_previews_never_see_the_subscription(self):
        # /sub/ is fetched by the client app itself; a preview of it would only leak the profile
        r = self.client.get(f"/sub/{self.secret}", headers={"User-Agent": "TelegramBot (like TwitterBot)"})
        self.assertEqual(r.status_code, 404)
        self.assertEqual(r.content, b"")

    def test_rotated_secret_invalidates_old_sub_url(self):
        old = self.secret
        self.store.rotate_secret(self.device.id)
        self.assertEqual(self.client.get(f"/sub/{old}").status_code, 404)
        new = self.store.subscription_secret(self.device.id)
        self.assertEqual(self.client.get(f"/sub/{new}").status_code, 200)
        token = self.store.create_token(self.device.id)
        loc = self.client.get(f"/e/{token}").headers["location"]
        self.assertIn(new, loc)
        self.assertNotIn(old, loc)

    def test_bad_tokens_are_indistinguishable_from_any_404(self):
        expired = self.store.create_token(self.device.id, ttl=10)
        self.now += 100
        baseline = self.client.get("/no/such/path")
        self.assertEqual(baseline.status_code, 404)
        for path in ("/e/unknown", f"/e/{expired}"):
            r = self.client.get(path)
            self.assertEqual(r.status_code, 404)
            self.assertEqual(r.content, baseline.content)
            self.assertEqual(r.headers.get("content-type"), baseline.headers.get("content-type"))

    def test_sub_serves_profile_then_404_after_revoke(self):
        r = self.client.get(f"/sub/{self.secret}")
        self.assertEqual(r.status_code, 200)
        self.assertTrue(r.headers["content-type"].startswith("application/json"))
        self.assertEqual(r.headers["profile-update-interval"], "12")
        vless = [o for o in r.json()["outbounds"] if o["type"] == "vless"]
        self.assertEqual(len(vless), 3)
        self.store.revoke_device(self.device.id)
        gone = self.client.get(f"/sub/{self.secret}")
        self.assertEqual(gone.status_code, 404)
        self.assertEqual(gone.content, b"")

    def test_unknown_secret_is_404(self):
        self.assertEqual(self.client.get("/sub/nope").status_code, 404)

    def test_oversized_ids_and_trailing_slashes_are_plain_404(self):
        baseline = self.client.get("/no/such/path")
        for path in ("/sub/" + "9" * 25 + ".x", "/sub/" + "9" * 5000 + ".x", "/e/x/", "/sub/x/",
                     "//sub/x", "//e/x", f"/sub/{self.secret}/"):
            r = self.client.get(path)
            self.assertEqual(r.status_code, 404, path[:40])
            self.assertEqual(r.content, baseline.content, path[:40])
            self.assertEqual(r.headers, baseline.headers, path[:40])

    def test_nothing_else_is_exposed(self):
        for path in ("/docs", "/redoc", "/openapi.json", "/", "/healthz", "/nodes"):
            r = self.client.get(path)
            self.assertEqual(r.status_code, 404, path)
            self.assertEqual(r.content, b"", path)

    def test_other_methods_are_404_with_empty_body(self):
        token = self.store.create_token(self.device.id)
        for method, path in (("post", "/e/x"), ("post", f"/e/{token}"), ("put", f"/sub/{self.secret}"),
                             ("delete", "/"), ("head", "/e/x")):
            r = getattr(self.client, method)(path)
            self.assertEqual(r.status_code, 404, (method, path))
            self.assertEqual(r.content, b"", (method, path))
        # the POST above must not have consumed the token
        self.assertEqual(self.client.get(f"/e/{token}").status_code, 302)

    def test_no_cookies(self):
        token = self.store.create_token(self.device.id)
        r = self.client.get(f"/e/{token}")
        self.assertNotIn("set-cookie", r.headers)

    def test_from_env_needs_no_import_time_env(self):
        import json

        from app.enroll import public

        self.assertFalse(hasattr(public, "app"))
        cfg_path = os.path.join(self.tmp.name, "cfg.json")
        with open(cfg_path, "w") as f:
            json.dump({"host": "h.example.com", "public_key": "K", "short_id": "s", "sni": "x.example.com"}, f)
        old = dict(os.environ)
        os.environ.pop("ENROLL_SECRET_KEY", None)
        os.environ["ENROLL_DB"] = os.path.join(self.tmp.name, "e.db")
        os.environ["ENROLL_CONFIG"] = cfg_path
        try:
            with self.assertRaises(RuntimeError):
                public.create_app_from_env()
            os.environ["ENROLL_SECRET_KEY"] = "test-key-0123456789abcdef"
            app = public.create_app_from_env()
        finally:
            os.environ.clear()
            os.environ.update(old)
        self.assertEqual(TestClient(app).get("/").status_code, 404)


if __name__ == "__main__":
    unittest.main()

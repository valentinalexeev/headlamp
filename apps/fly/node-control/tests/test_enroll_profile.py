import unittest

import _helpers  # noqa: F401
from app.enroll.profile import IngressCfg, cfg_from_dict, deep_link, render_profile, sub_url
from app.enroll.store import Device

CFG = IngressCfg(host="ingress.example.com", public_key="PUBKEY", short_id="ab12", sni="www.example.com")
UUIDS = {
    "auto": "11111111-1111-4111-8111-111111111111",
    "direct": "22222222-2222-4222-8222-222222222222",
    "exit": "33333333-3333-4333-8333-333333333333",
}


def _device(uuids=None):
    return Device(id=1, name="phone", owner="alice", client="sing-box",
                  expires_at=2_000_000_000, revoked=False, uuids=dict(UUIDS if uuids is None else uuids))


class DeepLinkTest(unittest.TestCase):
    def test_sing_box(self):
        self.assertEqual(
            deep_link("sing-box", "https://h/sub/x", "n"),
            "sing-box://import-remote-profile?url=https%3A%2F%2Fh%2Fsub%2Fx#n")

    def test_karing(self):
        self.assertEqual(
            deep_link("karing", "https://h/sub/x", "n"),
            "karing://install-config?url=https%3A%2F%2Fh%2Fsub%2Fx&name=n")

    def test_name_is_encoded(self):
        self.assertTrue(deep_link("karing", "https://h/s", "a b").endswith("&name=a%20b"))

    def test_unknown_client(self):
        with self.assertRaises(ValueError):
            deep_link("v2rayn", "https://h/sub/x", "n")

    def test_sub_url(self):
        self.assertEqual(sub_url(CFG, "sec"), "https://ingress.example.com/sub/sec")


class ProfileTest(unittest.TestCase):
    def setUp(self):
        self.p = render_profile(_device(), CFG)
        self.out = {o["tag"]: o for o in self.p["outbounds"]}

    def test_three_vless_with_device_uuids(self):
        vless = [o for o in self.p["outbounds"] if o["type"] == "vless"]
        self.assertEqual(len(vless), 3)
        self.assertEqual({o["tag"]: o["uuid"] for o in vless}, UUIDS)
        self.assertEqual(len({o["uuid"] for o in vless}), 3)

    def test_vless_reality_fields(self):
        for tag in UUIDS:
            o = self.out[tag]
            self.assertEqual(o["server"], "ingress.example.com")
            self.assertEqual(o["server_port"], 443)
            self.assertEqual(o["flow"], "xtls-rprx-vision")
            tls = o["tls"]
            self.assertTrue(tls["enabled"])
            self.assertEqual(tls["server_name"], "www.example.com")
            self.assertEqual(tls["utls"], {"enabled": True, "fingerprint": "chrome"})
            self.assertEqual(tls["reality"], {"enabled": True, "public_key": "PUBKEY", "short_id": "ab12"})

    def test_selector(self):
        sel = self.out["select"]
        self.assertEqual(sel["type"], "selector")
        self.assertEqual(sel["outbounds"], ["auto", "direct", "exit"])
        self.assertEqual(sel["default"], "auto")

    def test_plain_outbound_does_not_clash_with_a_mode_tag(self):
        self.assertEqual(self.out["plain"]["type"], "direct")
        self.assertEqual(self.out["direct"]["type"], "vless")  # the `direct` MODE is a VLESS outbound
        tags = [o["tag"] for o in self.p["outbounds"]]
        self.assertEqual(len(tags), len(set(tags)))

    def test_tun_route(self):
        tun = self.p["inbounds"][0]
        self.assertEqual(tun["type"], "tun")
        self.assertEqual(tun["address"], ["172.19.0.1/30", "fdfe:dcba:9876::1/126"])
        self.assertTrue(tun["auto_route"])
        self.assertTrue(tun["strict_route"])
        self.assertEqual(self.p["log"]["level"], "warn")
        r = self.p["route"]
        self.assertEqual(r["final"], "select")
        self.assertTrue(r["auto_detect_interface"])
        self.assertEqual(r["rules"], [{"action": "sniff"}, {"protocol": "dns", "action": "hijack-dns"}])

    def test_dns_goes_through_the_selected_outbound(self):
        self.assertEqual(self.p["dns"], {
            "servers": [
                {"tag": "remote", "address": "tls://1.1.1.1", "detour": "select"},
                {"tag": "local", "address": "local"},
            ],
            "rules": [{"outbound": "any", "server": "local"}],
            "final": "remote",
            "strategy": "prefer_ipv4",
        })
        self.assertIn({"protocol": "dns", "action": "hijack-dns"}, self.p["route"]["rules"])

    def test_dns_servers_and_final(self):
        dns = self.p["dns"]
        servers = {s["tag"]: s for s in dns["servers"]}
        self.assertEqual(servers["remote"], {"tag": "remote", "address": "tls://1.1.1.1", "detour": "select"})
        self.assertEqual(servers["local"], {"tag": "local", "address": "local"})
        self.assertNotIn("detour", servers["local"])
        self.assertEqual(dns["final"], "remote")
        self.assertEqual(dns["rules"], [{"outbound": "any", "server": "local"}])

    def test_outbound_server_lookup_does_not_depend_on_an_outbound(self):
        # No resolution loop: the VLESS outbounds' server domain is resolved by a server with no detour.
        dns = self.p["dns"]
        rule = next(r for r in dns["rules"] if r.get("outbound") == "any")
        server = next(s for s in dns["servers"] if s["tag"] == rule["server"])
        self.assertEqual(server["tag"], "local")
        self.assertNotIn("detour", server)

    def test_profile_has_no_name_or_owner(self):
        import json

        raw = json.dumps(render_profile(Device(id=1, name="Anna Phone", owner="anna.o", client="karing",
                                               expires_at=2_000_000_000, revoked=False, uuids=dict(UUIDS)), CFG))
        self.assertNotIn("Anna", raw)
        self.assertNotIn("anna.o", raw)

    def test_custom_port(self):
        cfg = IngressCfg(host="h.example.com", public_key="P", short_id="ab", sni="s.example.com", port=8443)
        for o in render_profile(_device(), cfg)["outbounds"]:
            if o["type"] == "vless":
                self.assertEqual(o["server_port"], 8443)

    def test_missing_mode_raises(self):
        u = dict(UUIDS)
        del u["exit"]
        with self.assertRaises(ValueError):
            render_profile(_device(u), CFG)


class CfgFromDictTest(unittest.TestCase):
    BASE = {"host": "h.example.com", "public_key": "P", "short_id": "ab", "sni": "s.example.com"}

    def test_defaults(self):
        cfg = cfg_from_dict(dict(self.BASE))
        self.assertEqual(cfg.port, 443)
        self.assertEqual(cfg.profile_name, "tunnel")
        self.assertEqual(cfg.host, "h.example.com")

    def test_port_and_profile_name(self):
        cfg = cfg_from_dict(dict(self.BASE, port=8443, profile_name="vpn"))
        self.assertEqual((cfg.port, cfg.profile_name), (8443, "vpn"))

    def test_invalid_values(self):
        for bad in ({"port": 0}, {"port": 65536}, {"port": "443"}, {"port": True}, {"profile_name": ""},
                    {"profile_name": 5}):
            with self.assertRaises(ValueError, msg=bad):
                cfg_from_dict(dict(self.BASE, **bad))
        for k in self.BASE:
            raw = dict(self.BASE)
            del raw[k]
            with self.assertRaises(ValueError, msg=k):
                cfg_from_dict(raw)


if __name__ == "__main__":
    unittest.main()

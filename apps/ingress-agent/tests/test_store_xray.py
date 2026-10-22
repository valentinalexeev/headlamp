import copy
import hashlib
import json
import os
import unittest

from _helpers import BASE, NOW, U1, U2, U3, FakeProc, cred
from app.store import Cred, diff, load, save, sweep
from app.xray import XrayRunner, email_for, parse_list, render_config


class StoreTests(unittest.TestCase):
    def test_diff_identical_sets(self):
        a = [cred(U1), cred(U2, "direct")]
        self.assertEqual(diff(a, list(reversed(a))), ([], []))

    def test_diff_added_and_removed(self):
        added, removed = diff([cred(U1), cred(U2)], [cred(U2), cred(U3)])
        self.assertEqual(added, [cred(U3)])
        self.assertEqual(removed, [U1])

    def test_diff_mode_change_is_remove_plus_add(self):
        added, removed = diff([cred(U1, "auto")], [cred(U1, "direct")])
        self.assertEqual((added, removed), ([cred(U1, "direct")], [U1]))

    def test_sweep_drops_expired(self):
        keep = cred(U1, not_after=NOW + 1)
        self.assertEqual(sweep([keep, cred(U2, not_after=NOW), cred(U3, not_after=NOW - 5)], NOW), [keep])

    def test_save_load_roundtrip_mode_0600(self):
        import tempfile
        from pathlib import Path

        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "sub" / "credentials.json"
            save(p, [cred(U1)])
            self.assertEqual(load(p), [cred(U1)])
            self.assertEqual(os.stat(p).st_mode & 0o777, 0o600)
            self.assertEqual(load(Path(d) / "missing.json"), [])


class RenderTests(unittest.TestCase):
    def test_email_is_deterministic_and_opaque(self):
        e = email_for(U1)
        self.assertEqual(e, "u" + hashlib.sha256(U1.encode()).hexdigest()[:12] + "@i")
        self.assertNotIn(U1, e)

    def test_rule_order(self):
        creds = [cred(U1, "direct"), cred(U2, "exit"), cred(U3, "auto")]
        cfg = render_config(BASE, creds, direct_geoip=("xx", "yy"), direct_geosite=("zz",))
        rules = cfg["routing"]["rules"]
        e = email_for
        self.assertEqual(rules[0], {"type": "field", "inboundTag": ["reality-in"], "ip": ["geoip:private", "100.64.0.0/10"], "outboundTag": "blocked"})
        self.assertEqual(rules[1], {"type": "field", "user": [e(U1)], "outboundTag": "direct"})
        self.assertEqual(rules[2], {"type": "field", "user": [e(U2)], "outboundTag": "via-exit"})
        self.assertEqual(rules[3], {"type": "field", "user": [e(U3)], "ip": ["geoip:xx", "geoip:yy"], "outboundTag": "direct"})
        self.assertEqual(rules[4], {"type": "field", "user": [e(U3)], "domain": ["geosite:zz"], "outboundTag": "direct"})
        self.assertEqual(rules[5], {"type": "field", "user": [e(U3)], "outboundTag": "via-exit"})
        self.assertEqual(rules[6:], BASE["routing"]["rules"])
        clients = cfg["inbounds"][1]["settings"]["clients"]
        self.assertEqual(clients[0], {"id": U1, "email": e(U1), "flow": "xtls-rprx-vision"})
        self.assertEqual(len(clients), 3)

    def test_block_rule_first_with_zero_creds(self):
        rules = render_config(BASE, [])["routing"]["rules"]
        self.assertEqual(len(rules), 2)
        self.assertEqual(rules[0]["outboundTag"], "blocked")
        self.assertEqual(rules[1:], BASE["routing"]["rules"])

    def test_domain_strategy_forced(self):
        for ds in (None, "AsIs", "IPIfNonMatch"):
            base = copy.deepcopy(BASE)
            if ds:
                base["routing"]["domainStrategy"] = ds
            self.assertEqual(render_config(base, [])["routing"]["domainStrategy"], "IPOnDemand")

    def test_api_rule_untouched_after_block(self):
        rules = render_config(BASE, [cred(U1)])["routing"]["rules"]
        self.assertEqual(rules[-1], BASE["routing"]["rules"][0])
        self.assertNotIn("api", rules[0].get("inboundTag", []))

    def test_only_present_modes_get_rules(self):
        rules = render_config(BASE, [cred(U1, "exit")])["routing"]["rules"]
        self.assertEqual([r["outboundTag"] for r in rules], ["blocked", "via-exit", "api"])

    def test_does_not_mutate_input(self):
        before = copy.deepcopy(BASE)
        render_config(BASE, [cred(U1)])
        self.assertEqual(BASE, before)

    def test_missing_inbound_or_routing(self):
        no_in = {**BASE, "inbounds": [BASE["inbounds"][0]]}
        with self.assertRaises(ValueError):
            render_config(no_in, [])
        with self.assertRaises(ValueError):
            render_config({"inbounds": BASE["inbounds"]}, [])


class RunnerTests(unittest.TestCase):
    def test_commands(self):
        p = FakeProc()
        r = XrayRunner("/bin/xray", "xray", "127.0.0.1:10085", run=p)
        self.assertTrue(r.test_config("/x/c.json"))
        r.restart()
        self.assertEqual(p.calls[0], ["/bin/xray", "run", "-test", "-c", "/x/c.json"])
        self.assertEqual(p.calls[1], ["systemctl", "restart", "xray"])

    def test_test_config_false(self):
        r = XrayRunner("/bin/xray", "xray", "a:1", run=FakeProc(test_ok=False))
        self.assertFalse(r.test_config("/x"))

    def test_stats_survives_oserror_and_timeout(self):
        import subprocess

        for err in (OSError("x"), subprocess.TimeoutExpired("xray", 1)):
            p = FakeProc()
            p.stats_error = err
            self.assertEqual(XrayRunner("/bin/xray", "xray", "a:1", run=p).stats(), {})

    def test_stats_parse(self):
        out = json.dumps({"stat": [
            {"name": "user>>>a@i>>>traffic>>>uplink", "value": "5"},
            {"name": "user>>>a@i>>>traffic>>>downlink", "value": "7"},
            {"name": "user>>>b@i>>>traffic>>>downlink", "value": 9},
        ]})
        p = FakeProc(stats_out=out)
        r = XrayRunner("/bin/xray", "xray", "127.0.0.1:10085", run=p)
        self.assertEqual(r.stats(), {"a@i": {"up": 5, "down": 7}, "b@i": {"up": 0, "down": 9}})
        self.assertEqual(p.calls[0], ["/bin/xray", "api", "statsquery", "-s", "127.0.0.1:10085", "-pattern", "user>>>"])


if __name__ == "__main__":
    unittest.main()


class DirectListTests(unittest.TestCase):
    def test_auto_without_lists_uses_the_exit_for_everything(self):
        rules = render_config(BASE, [cred(U3, "auto")])["routing"]["rules"]
        self.assertEqual(rules[1:-len(BASE["routing"]["rules"]) or None][0], {"type": "field", "user": [email_for(U3)], "outboundTag": "via-exit"})
        self.assertFalse(any(r.get("outboundTag") == "direct" and r.get("user") for r in rules))

    def test_only_geoip_list(self):
        rules = render_config(BASE, [cred(U3, "auto")], direct_geoip=("xx",))["routing"]["rules"]
        self.assertEqual(rules[1], {"type": "field", "user": [email_for(U3)], "ip": ["geoip:xx"], "outboundTag": "direct"})
        self.assertEqual(rules[2], {"type": "field", "user": [email_for(U3)], "outboundTag": "via-exit"})

    def test_invalid_list_entries_are_rejected(self):
        for bad in ("a b", "x;y", "", "a" * 65, "x\ny", 'x"y'):
            with self.assertRaises(ValueError):
                render_config(BASE, [], direct_geoip=(bad,))
            with self.assertRaises(ValueError):
                render_config(BASE, [], direct_geosite=(bad,))

    def test_parse_list(self):
        self.assertEqual(parse_list(""), [])
        self.assertEqual(parse_list(" xx, yy ,,zz "), ["xx", "yy", "zz"])

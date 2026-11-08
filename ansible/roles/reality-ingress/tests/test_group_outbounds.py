"""Renders config.base.json.j2 with real ansible (#139) and checks the per-group outbounds of the tailnet grants.

Run: python3 -m unittest ansible/roles/reality-ingress/tests/test_group_outbounds.py (needs ansible-playbook; skips
without it). group-outbounds.sh runs it for `make test-ansible-roles`.
"""
import json
import os
import shutil
import subprocess
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROLE = os.path.dirname(HERE)
BASE_VARS = {
    "ingress_listen_port": 443, "reality_dest": "front.example:443", "reality_sni": "front.example",
    "reality_private_key": "PRIV", "reality_short_id": "ab", "socks_listen": "127.0.0.1:1055",
    "xray_api_listen": "127.0.0.1:10085", "hy2_enabled": False,
}


def render(extra):
    """Renders the template through ansible and returns the parsed JSON."""
    with tempfile.TemporaryDirectory() as tmp:
        out = os.path.join(tmp, "out.json")
        play = os.path.join(tmp, "play.yml")
        with open(play, "w") as f:
            json.dump([{"hosts": "localhost", "gather_facts": False, "tasks": [{"ansible.builtin.template": {
                "src": os.path.join(ROLE, "templates", "config.base.json.j2"), "dest": out}}]}], f)
        varsf = os.path.join(tmp, "vars.json")
        with open(varsf, "w") as f:
            json.dump({**BASE_VARS, **extra}, f)
        defaults = os.path.join(ROLE, "defaults", "main.yml")
        r = subprocess.run(["ansible-playbook", play, "-i", "localhost,", "-c", "local", "-e", "@" + defaults,
                            "-e", "@" + varsf], capture_output=True, text=True, timeout=120)
        if r.returncode != 0:
            raise AssertionError(r.stdout[-2000:] + r.stderr[-2000:])
        with open(out) as f:
            text = f.read()
    start = text.index("{")
    return json.loads(text[start:])


@unittest.skipUnless(shutil.which("ansible-playbook"), "ansible-playbook is not installed")
class GroupOutbounds(unittest.TestCase):
    def outbounds(self, cfg):
        return {o["tag"]: o for o in cfg["outbounds"]}

    def test_no_groups_keeps_phase_one(self):
        outs = self.outbounds(render({}))
        self.assertIn("via-tailnet", outs)
        self.assertFalse([t for t in outs if t.startswith("via-tailnet-")])

    def test_one_outbound_per_group_in_order(self):
        groups = [{"id": "g-0a1b2c3d", "authkey": "never-read"}, {"id": "g-deadbeef"}]
        outs = self.outbounds(render({"socks_grant_groups": groups}))
        self.assertEqual(outs["via-tailnet-g-0a1b2c3d"]["settings"]["servers"], [{"address": "127.0.0.1", "port": 1056}])
        self.assertEqual(outs["via-tailnet-g-deadbeef"]["settings"]["servers"], [{"address": "127.0.0.1", "port": 1057}])
        self.assertEqual(outs["via-tailnet"]["settings"]["servers"][0]["port"], 1055)
        self.assertNotIn("never-read", json.dumps(outs))

    def test_port_base(self):
        outs = self.outbounds(render({"socks_grant_groups": [{"id": "g-0a1b2c3d"}], "socks_grant_port_base": 2000}))
        self.assertEqual(outs["via-tailnet-g-0a1b2c3d"]["settings"]["servers"][0]["port"], 2000)

    def test_block_stays_first(self):
        cfg = render({"socks_grant_groups": [{"id": "g-0a1b2c3d"}]})
        self.assertEqual(cfg["outbounds"][0]["tag"], "blocked")


if __name__ == "__main__":
    unittest.main()

import json
import os
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from app.main import Applier  # noqa: E402
from app.store import Cred  # noqa: E402
from app.xray import XrayRunner  # noqa: E402

TOKEN = "t0ken-for-tests"
NOW = 1_000_000
U1 = "11111111-1111-4111-8111-111111111111"
U2 = "22222222-2222-4222-8222-222222222222"
U3 = "33333333-3333-4333-8333-333333333333"
BASE = {
    "inbounds": [
        {"tag": "api", "port": 10085, "settings": {}},
        {"tag": "reality-in", "port": 443, "protocol": "vless", "settings": {"clients": [], "decryption": "none"}},
    ],
    "outbounds": [{"tag": "direct"}, {"tag": "blocked"}, {"tag": "via-exit"}],
    "routing": {"rules": [{"type": "field", "inboundTag": ["api"], "outboundTag": "api"}]},
}


def cred(uuid, mode="auto", not_after=NOW + 1000):
    return Cred(uuid, mode, not_after)


def body(*creds):
    return {"credentials": [{"uuid": c.uuid, "mode": c.mode, "not_after": c.not_after} for c in creds]}


class FakeProc:
    """Stands in for the subprocess runner: records calls, never executes anything."""

    def __init__(self, test_ok=True, stats_out="{}"):
        self.restart_failures = 0
        self.stats_error = None
        self.calls = []
        self.test_ok = test_ok
        self.stats_out = stats_out

    def __call__(self, args):
        self.calls.append(list(args))
        if "-test" in args:
            return (0 if self.test_ok else 1), ""
        if "statsquery" in args:
            if self.stats_error:
                raise self.stats_error
            return 0, self.stats_out
        if args[0] == "systemctl" and self.restart_failures:
            self.restart_failures -= 1
            raise TimeoutError("restart timed out")
        return 0, ""

    def kinds(self):
        out = []
        for a in self.calls:
            out.append("test" if "-test" in a else "stats" if "statsquery" in a else "restart")
        return out


class Env:
    def __init__(self, test_ok=True, stats_out="{}"):
        self._tmp = tempfile.TemporaryDirectory()
        d = Path(self._tmp.name)
        self.state = d / "state" / "credentials.json"
        self.base = d / "config.base.json"
        self.config = d / "config.json"
        self.base.write_text(json.dumps(BASE))
        self.proc = FakeProc(test_ok, stats_out)
        self.now = NOW
        self.runner = XrayRunner("/bin/xray", "xray", "127.0.0.1:10085", run=self.proc)
        self.applier = Applier(self.state, self.base, self.config, self.runner, clock=lambda: self.now)

    def cleanup(self):
        self._tmp.cleanup()

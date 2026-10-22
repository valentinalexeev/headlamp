"""Render the xray config from opaque credentials and drive the xray process."""
import copy
import hashlib
import json
import subprocess
import re
from typing import Callable, Sequence

from .store import Cred

INBOUND_TAG = "reality-in"
FLOW = "xtls-rprx-vision"
BLOCKED_IPS = ["geoip:private", "100.64.0.0/10"]
_LIST_ENTRY = re.compile(r"^[A-Za-z0-9!@._-]{1,64}$")


def email_for(uuid: str) -> str:
    return "u" + hashlib.sha256(uuid.encode()).hexdigest()[:12] + "@i"


def parse_list(value: str) -> list[str]:
    """Comma-separated list from an environment variable; empty entries dropped."""
    return [x.strip() for x in (value or "").split(",") if x.strip()]


def render_config(base: dict, creds: list[Cred], direct_geoip: Sequence[str] = (), direct_geosite: Sequence[str] = ()) -> dict:
    """`direct_geoip` / `direct_geosite`: geodata list names whose destinations leave from this host in `auto` mode."""
    for entry in (*direct_geoip, *direct_geosite):
        if not _LIST_ENTRY.match(entry):
            raise ValueError(f"invalid direct list entry: {entry!r}")
    cfg = copy.deepcopy(base)
    inbound = next((i for i in cfg.get("inbounds", []) if i.get("tag") == INBOUND_TAG), None)
    if inbound is None:
        raise ValueError(f"base config has no inbound tagged {INBOUND_TAG}")
    if "routing" not in cfg:
        raise ValueError("base config has no routing section")
    inbound.setdefault("settings", {}).setdefault("clients", []).extend(
        {"id": c.uuid, "email": email_for(c.uuid), "flow": FLOW} for c in creds
    )

    def users(mode: str) -> list[str]:
        return [email_for(c.uuid) for c in creds if c.mode == mode]

    cfg["routing"]["domainStrategy"] = "IPOnDemand"  # domain destinations must resolve before the block rule
    rules: list[dict] = [{"type": "field", "inboundTag": [INBOUND_TAG], "ip": list(BLOCKED_IPS), "outboundTag": "blocked"}]
    if u := users("direct"):
        rules.append({"type": "field", "user": u, "outboundTag": "direct"})
    if u := users("exit"):
        rules.append({"type": "field", "user": u, "outboundTag": "via-exit"})
    if u := users("auto"):
        if direct_geoip:
            rules.append({"type": "field", "user": u, "ip": [f"geoip:{x}" for x in direct_geoip], "outboundTag": "direct"})
        if direct_geosite:
            rules.append({"type": "field", "user": u, "domain": [f"geosite:{x}" for x in direct_geosite], "outboundTag": "direct"})
        rules.append({"type": "field", "user": u, "outboundTag": "via-exit"})
    cfg["routing"]["rules"] = rules + cfg["routing"].get("rules", [])
    return cfg


def _subprocess_run(args: list[str]) -> tuple[int, str]:
    p = subprocess.run(args, capture_output=True, text=True, timeout=30)
    return p.returncode, p.stdout


class XrayRunner:
    def __init__(self, bin: str, unit: str, api_addr: str, run: Callable[[list[str]], tuple[int, str]] = _subprocess_run):
        self.bin, self.unit, self.api_addr, self._run = bin, unit, api_addr, run

    def test_config(self, path) -> bool:
        return self._run([self.bin, "run", "-test", "-c", str(path)])[0] == 0

    def restart(self) -> None:
        code, _ = self._run(["systemctl", "restart", self.unit])
        if code != 0:
            raise RuntimeError("xray restart failed")

    def stats(self) -> dict[str, dict[str, int]]:
        try:
            code, out = self._run([self.bin, "api", "statsquery", "-s", self.api_addr, "-pattern", "user>>>"])
        except (OSError, subprocess.TimeoutExpired):
            return {}
        if code != 0:
            return {}
        result: dict[str, dict[str, int]] = {}
        try:
            entries = json.loads(out or "{}").get("stat") or []
        except ValueError:
            return {}
        for e in entries:
            parts = str(e.get("name", "")).split(">>>")
            if len(parts) != 4 or parts[0] != "user":
                continue
            slot = result.setdefault(parts[1], {"up": 0, "down": 0})
            key = "up" if parts[3] == "uplink" else "down" if parts[3] == "downlink" else None
            if key:
                slot[key] = int(e.get("value") or 0)
        return result

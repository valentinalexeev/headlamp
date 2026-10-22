"""Render the sing-box profile for a device and the client import deep links.

Nothing here carries the device name or owner: the deep link and the profile both pass through
the ingress host, which must not learn who the clients are. The deep link label is the neutral
``IngressCfg.profile_name`` (``ENROLL_CONFIG`` field ``profile_name``, default ``"tunnel"``).

DNS: the profile hijacks DNS on the tun and resolves through a DoT server reached via the
``select`` outbound, so lookups follow the selected mode (no plaintext DNS to the local network). The one
exception is the ingress host's own name, resolved by the local resolver (rule ``outbound: any``).
"""
from dataclasses import dataclass
from urllib.parse import quote

from .store import MODES, Device

TUN_ADDRESSES = ["172.19.0.1/30", "fdfe:dcba:9876::1/126"]
# sing-box legacy DNS server format (valid through 1.13).
DNS = {
    "servers": [
        {"tag": "remote", "address": "tls://1.1.1.1", "detour": "select"},
        {"tag": "local", "address": "local"},
    ],
    # Domain names of outbound servers (the ingress host) use the local resolver: resolving them through
    # ``remote`` would need the very outbound that needs the lookup.
    "rules": [{"outbound": "any", "server": "local"}],
    "final": "remote",
    "strategy": "prefer_ipv4",
}


@dataclass(frozen=True)
class IngressCfg:
    host: str
    public_key: str
    short_id: str
    sni: str
    port: int = 443
    profile_name: str = "tunnel"


def cfg_from_dict(raw: dict) -> IngressCfg:
    """Build an IngressCfg from the ENROLL_CONFIG JSON. Raises ValueError on a bad file."""
    missing = [k for k in ("host", "public_key", "short_id", "sni") if not isinstance(raw.get(k), str) or not raw[k]]
    if missing:
        raise ValueError(f"ENROLL_CONFIG: missing or empty: {', '.join(missing)}")
    port = raw.get("port", 443)
    if isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535:
        raise ValueError("ENROLL_CONFIG: port must be an integer 1-65535")
    name = raw.get("profile_name", "tunnel")
    if not isinstance(name, str) or not name:
        raise ValueError("ENROLL_CONFIG: profile_name must be a non-empty string")
    return IngressCfg(host=raw["host"], public_key=raw["public_key"], short_id=raw["short_id"], sni=raw["sni"],
                      port=port, profile_name=name)


def sub_url(cfg: IngressCfg, secret: str) -> str:
    return f"https://{cfg.host}/sub/{secret}"


def deep_link(client: str, sub_url: str, name: str) -> str:
    url = quote(sub_url, safe="")
    label = quote(name, safe="")
    if client == "sing-box":
        return f"sing-box://import-remote-profile?url={url}#{label}"
    if client == "karing":
        return f"karing://install-config?url={url}&name={label}"
    raise ValueError(f"unknown client: {client!r}")


def render_profile(device: Device, cfg: IngressCfg) -> dict:
    missing = [m for m in MODES if not device.uuids.get(m)]
    if missing:
        raise ValueError(f"device lacks uuids for modes: {', '.join(missing)}")
    vless = [
        {
            "type": "vless",
            "tag": mode,
            "server": cfg.host,
            "server_port": cfg.port,
            "uuid": device.uuids[mode],
            "flow": "xtls-rprx-vision",
            "tls": {
                "enabled": True,
                "server_name": cfg.sni,
                "utls": {"enabled": True, "fingerprint": "chrome"},
                "reality": {"enabled": True, "public_key": cfg.public_key, "short_id": cfg.short_id},
            },
        }
        for mode in MODES
    ]
    return {
        "log": {"level": "warn"},
        "inbounds": [
            {"type": "tun", "address": list(TUN_ADDRESSES), "auto_route": True, "strict_route": True}
        ],
        "dns": {**DNS, "servers": [dict(s) for s in DNS["servers"]], "rules": [dict(r) for r in DNS["rules"]]},
        "outbounds": [
            {"type": "selector", "tag": "select", "outbounds": list(MODES), "default": "auto"},
            *vless,
            {"type": "direct", "tag": "plain"},
        ],
        "route": {
            "rules": [{"action": "sniff"}, {"protocol": "dns", "action": "hijack-dns"}],
            "final": "select",
            "auto_detect_interface": True,
        },
    }

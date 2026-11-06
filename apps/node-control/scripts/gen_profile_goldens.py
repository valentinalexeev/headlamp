#!/usr/bin/env python3
"""Generate the enrolment profile goldens with the Python renderer (needs python3, stdlib only).

Usage: gen_profile_goldens.py <python node-control dir> <output testdata dir>
All hosts, keys, names and uuids below are fake.

Serialisation matches the Python service: the sing-box profile goes through Starlette's JSONResponse
(ensure_ascii=False, separators=(",", ":")); the Clash profile through plain json.dumps(...) (defaults).
"""
import json
import os
import sys

src, out = sys.argv[1], sys.argv[2]
sys.path.insert(0, src)
from app.enroll.profile import (IngressCfg, cfg_from_dict, deep_link, render_clash_profile,  # noqa: E402
                                render_profile, sub_url)
from app.enroll.store import Device  # noqa: E402

UUIDS = {
    "auto": "11111111-1111-4111-8111-111111111111",
    "direct": "22222222-2222-4222-8222-222222222222",
    "exit": "33333333-3333-4333-8333-333333333333",
}
CASES = {
    "": dict(
        cfg=IngressCfg(host="ingress.example.com", public_key="AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE",
                       short_id="abcd1234", sni="ingress.example.com"),
        name="phone", owner="alice"),
    "custom": dict(
        cfg=IngressCfg(host="edge.example.org", public_key="ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210-_ZyXwV",
                       short_id="abcd1234", sni="front.example.org", port=8443, profile_name="vpn"),
        name="tablet", owner="bob"),
    "unusual": dict(
        cfg=IngressCfg(host="ingress.example.com", public_key="AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE",
                       short_id="abcd1234", sni="ingress.example.com"),
        name="Zoë O'Brien <script> \U0001f600", owner='o"wn\\er\né'),
    "escape": dict(
        cfg=IngressCfg(host="ingress.example.com", public_key='k"<>&\\/ é\U0001f600\x01\x7f',
                       short_id="abcd1234", sni="bücher.example.com", profile_name="na me/é&x"),
        name="phone", owner="alice"),
}


def dev(name, owner):
    return Device(id=1, name=name, owner=owner, client="sing-box", expires_at=2_000_000_000, revoked=False,
                  uuids=dict(UUIDS))


expected = {}
for case, c in CASES.items():
    sfx = f"_{case}" if case else ""
    cfg, d = c["cfg"], dev(c["name"], c["owner"])
    sb = json.dumps(render_profile(d, cfg), ensure_ascii=False, allow_nan=False, indent=None,
                    separators=(",", ":")).encode("utf-8")
    cl = json.dumps(render_clash_profile(d, cfg)).encode("utf-8")
    with open(os.path.join(out, f"profile_singbox{sfx}.golden"), "wb") as f:
        f.write(sb)
    with open(os.path.join(out, f"profile_clash{sfx}.golden"), "wb") as f:
        f.write(cl)
    url = sub_url(cfg, "sec-ret_1")
    expected[case or "default"] = {
        "cfg": {"host": cfg.host, "public_key": cfg.public_key, "short_id": cfg.short_id, "sni": cfg.sni,
                "port": cfg.port, "profile_name": cfg.profile_name},
        "name": c["name"], "owner": c["owner"], "uuids": UUIDS,
        "secret": "sec-ret_1", "sub_url": url,
        "deep_links": {k: deep_link(k, url, cfg.profile_name) for k in ("sing-box", "karing", "clash")},
    }
base = {"host": "h.example.com", "public_key": "P", "short_id": "ab", "sni": "s.example.com"}
bad = [{"port": 0}, {"port": 65536}, {"port": "443"}, {"port": True}, {"port": 443.0}, {"port": None},
       {"profile_name": ""}, {"profile_name": 5}, {"profile_name": None}, {"host": ""}, {"host": 1}, {"sni": None}]
errors = []
for b in bad:
    raw = dict(base, **b)
    try:
        cfg_from_dict(raw)
        raise SystemExit(f"expected failure: {b}")
    except ValueError as e:
        errors.append({"json": json.dumps(raw), "error": str(e)})
raw = {k: v for k, v in base.items() if k not in ("host", "sni")}
try:
    cfg_from_dict(raw)
except ValueError as e:
    errors.append({"json": json.dumps(raw), "error": str(e)})
raw = {}
try:
    cfg_from_dict(raw)
except ValueError as e:
    errors.append({"json": json.dumps(raw), "error": str(e)})
expected["cfg_errors"] = errors
with open(os.path.join(out, "profile_expected.json"), "w") as f:
    json.dump(expected, f, indent=2, ensure_ascii=False)
    f.write("\n")

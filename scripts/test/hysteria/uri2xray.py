#!/usr/bin/env python3
"""Turns one link of a `uri` subscription (#278) into an xray-core client config, the way an xray app such as
Happ reads it: a SOCKS inbound on 127.0.0.1:2080 and one outbound. Only the fields of the link are used, so a link
that misses something xray needs fails here or in the e2e check. The one test addition: the test CA (the bridge
certificate is made for this run) as a trusted certificate, because xray uses its own CA bundle and does not read
SSL_CERT_FILE. Usage: uri2xray.py <subscription file> <tag> [<test CA file>]."""
import base64, json, sys
from urllib.parse import parse_qs, unquote, urlsplit

body = open(sys.argv[1], "rb").read()
links = base64.b64decode(body).decode().split()
want = sys.argv[2]
link = next((l for l in links if unquote(urlsplit(l).fragment) == want), None)
if link is None:
    sys.exit("no link " + want + " in " + ", ".join(unquote(urlsplit(l).fragment) for l in links))
u = urlsplit(link)
q = {k: v[0] for k, v in parse_qs(u.query).items()}
user = unquote(u.username or "")
if u.scheme == "vless":
    assert q.get("security") == "reality" and q.get("flow") == "xtls-rprx-vision", q
    out = {"protocol": "vless",
           "settings": {"vnext": [{"address": u.hostname, "port": u.port,
                                   "users": [{"id": user, "encryption": q.get("encryption", "none"), "flow": q["flow"]}]}]},
           "streamSettings": {"network": q.get("type", "tcp"), "security": "reality",
                              "realitySettings": {"serverName": q["sni"], "fingerprint": q.get("fp", "chrome"),
                                                  "publicKey": q["pbk"], "shortId": q.get("sid", "")}}}
elif u.scheme in ("hysteria2", "hy2"):
    assert q.get("obfs") == "salamander" and q.get("obfs-password"), q
    stream = {"network": "hysteria", "hysteriaSettings": {"version": 2, "auth": user},
              "security": "tls", "tlsSettings": {"serverName": q.get("sni", u.hostname),
                                                 "alpn": q.get("alpn", "h3").split(",")},
              "finalmask": {"udp": [{"type": "salamander", "settings": {"password": q["obfs-password"]}}]}}
    if len(sys.argv) > 3:
        stream["tlsSettings"]["certificates"] = [{"usage": "verify", "certificateFile": sys.argv[3]}]
    out = {"protocol": "hysteria", "settings": {"version": 2, "address": u.hostname, "port": u.port},
           "streamSettings": stream}
else:
    sys.exit("unknown scheme " + u.scheme)
out["tag"] = "out"
print(json.dumps({"log": {"loglevel": "warning"},
                  "inbounds": [{"listen": "127.0.0.1", "port": 2080, "protocol": "socks", "settings": {"udp": True}}],
                  "outbounds": [out]}))

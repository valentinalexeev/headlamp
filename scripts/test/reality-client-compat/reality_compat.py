#!/usr/bin/env python3
"""Reality client/server compatibility matrix on loopback. Needs no deployed infrastructure.

Starts a local Xray VLESS+Reality server (one per ``--xray-server`` binary), then connects each client
(xray, sing-box, mihomo; whichever binaries are given) with each TLS fingerprint and fetches ``--target``
through it. Prints a table (``ok``/``FAIL``) and exits 1 when the CONTROL (the xray client with the first
fingerprint against every server) fails, because then the lab itself is broken and the matrix means nothing.

    reality_compat.py --xray-server ./x-old/xray --xray-server ./x-new/xray --xray-client ./x-new/xray \\
        --singbox ./sing-box --mihomo ./mihomo --fingerprints chrome,firefox,safari

Binaries are never downloaded here: fetch them yourself and verify their checksums (see README.md).
The server's ``dest`` (default www.microsoft.com:443) must be reachable; ``--target`` is fetched
through the tunnel, so the server's own outbound needs internet access too.
"""
import argparse
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import time
import uuid

SERVER_PORT, XRAY_PORT, SB_PORT, MIHOMO_PORT = 10443, 10901, 10902, 10903
SHORT_ID = "abcd1234"


def write(path, text):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(text)


def spawn(cmd, log):
    return subprocess.Popen(cmd, stdout=open(log, "w"), stderr=subprocess.STDOUT)


def stop(*procs):
    for p in procs:
        p.send_signal(signal.SIGTERM)
    for p in procs:
        try:
            p.wait(5)
        except subprocess.TimeoutExpired:
            p.kill()


def x25519(xray):
    out = subprocess.run([xray, "x25519"], capture_output=True, text=True).stdout
    priv = re.search(r"Private ?[Kk]ey: *(\S+)", out).group(1)
    pub = re.search(r"(?:Public ?[Kk]ey[^:]*|Password[^:]*): *(\S+)", out).group(1)
    return priv, pub


def fetch(proxy_args, target):
    r = subprocess.run(["curl", "-s", "-m", "20", "-o", "/dev/null", "-w", "%{http_code}", *proxy_args, target],
                       capture_output=True, text=True)
    return r.stdout.strip() in ("200", "204", "301", "302")


def server_config(priv, uid, dest):
    host = dest.rsplit(":", 1)[0]
    return json.dumps({
        "log": {"loglevel": "warning"},
        "inbounds": [{"listen": "127.0.0.1", "port": SERVER_PORT, "protocol": "vless",
                      "settings": {"clients": [{"id": uid, "flow": "xtls-rprx-vision"}], "decryption": "none"},
                      "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {
                          "dest": dest, "serverNames": [host], "privateKey": priv, "shortIds": [SHORT_ID]}}}],
        "outbounds": [{"protocol": "freedom"}]})


def xray_client(uid, pub, host, fp):
    return json.dumps({
        "log": {"loglevel": "warning"},
        "inbounds": [{"listen": "127.0.0.1", "port": XRAY_PORT, "protocol": "socks"}],
        "outbounds": [{"protocol": "vless", "settings": {"vnext": [{"address": "127.0.0.1", "port": SERVER_PORT,
                       "users": [{"id": uid, "encryption": "none", "flow": "xtls-rprx-vision"}]}]},
                       "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {
                           "serverName": host, "fingerprint": fp, "publicKey": pub, "shortId": SHORT_ID}}}]})


def singbox_client(uid, pub, host, fp):
    return json.dumps({
        "log": {"level": "warn", "timestamp": False},
        "dns": {"servers": [{"type": "local", "tag": "l"}]},
        "inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": SB_PORT}],
        "outbounds": [{"type": "vless", "tag": "p", "server": "127.0.0.1", "server_port": SERVER_PORT, "uuid": uid,
                       "flow": "xtls-rprx-vision", "tls": {"enabled": True, "server_name": host,
                       "utls": {"enabled": True, "fingerprint": fp},
                       "reality": {"enabled": True, "public_key": pub, "short_id": SHORT_ID}}}],
        "route": {"final": "p", "default_domain_resolver": {"server": "l"}}})


def mihomo_client(uid, pub, host, fp):
    return (f"mixed-port: {MIHOMO_PORT}\nallow-lan: false\nmode: rule\nlog-level: warning\nproxies:\n"
            f"  - {{name: p, type: vless, server: 127.0.0.1, port: {SERVER_PORT}, uuid: {uid}, network: tcp, tls: true,\n"
            f"     udp: false, flow: xtls-rprx-vision, servername: {host}, client-fingerprint: {fp},\n"
            f"     reality-opts: {{public-key: {pub}, short-id: {SHORT_ID}}}}}\nrules:\n  - MATCH,p\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--xray-server", action="append", required=True, metavar="PATH", help="repeat per version")
    ap.add_argument("--xray-client", metavar="PATH", help="control client (default: the first server binary)")
    ap.add_argument("--singbox", metavar="PATH")
    ap.add_argument("--mihomo", metavar="PATH")
    ap.add_argument("--fingerprints", default="chrome", help="comma separated; the first is used for the control")
    ap.add_argument("--dest", default="www.microsoft.com:443")
    ap.add_argument("--target", default="https://example.com")
    a = ap.parse_args()
    fps = [f for f in a.fingerprints.split(",") if f]
    host = a.dest.rsplit(":", 1)[0]
    control_client = a.xray_client or a.xray_server[0]
    rows, control_ok = [], True
    with tempfile.TemporaryDirectory() as tmp:
        for srv in a.xray_server:
            ver = subprocess.run([srv, "version"], capture_output=True, text=True).stdout.split("\n")[0][:20]
            priv, pub = x25519(srv)
            uid = str(uuid.uuid4())
            write(f"{tmp}/s.json", server_config(priv, uid, a.dest))
            ps = spawn([srv, "run", "-c", f"{tmp}/s.json"], f"{tmp}/s.log")
            time.sleep(1.5)
            try:
                clients = [("xray(control)", control_client, xray_client, XRAY_PORT, "socks5h", lambda c, p: [c, "run", "-c", p], "json")]
                if a.singbox:
                    clients.append(("sing-box", a.singbox, singbox_client, SB_PORT, "socks5h", lambda c, p: [c, "run", "-c", p], "json"))
                if a.mihomo:
                    clients.append(("mihomo", a.mihomo, mihomo_client, MIHOMO_PORT, "http", lambda c, p: [c, "-f", p], "yaml"))
                for name, binary, make, port, scheme, cmd, ext in clients:
                    for fp in fps:
                        cfg = f"{tmp}/c.{ext}"
                        write(cfg, make(uid, pub, host, fp))
                        pc = spawn(cmd(binary, cfg), f"{tmp}/c.log")
                        time.sleep(2.5)
                        ok = fetch(["-x", f"{scheme}://127.0.0.1:{port}"], a.target)
                        stop(pc)
                        rows.append((ver, name, fp, "ok" if ok else "FAIL"))
                        if name == "xray(control)" and fp == fps[0] and not ok:
                            control_ok = False
            finally:
                stop(ps)
    w = max(len(r[1]) for r in rows)
    print(f"{'server':20s} {'client':{w}s} {'fingerprint':12s} result")
    for r in rows:
        print(f"{r[0]:20s} {r[1]:{w}s} {r[2]:12s} {r[3]}")
    if not control_ok:
        print("\nCONTROL FAILED: the xray client could not connect to the xray server; the lab is broken "
              "(dest/target unreachable, port in use?), so the matrix above is not meaningful.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

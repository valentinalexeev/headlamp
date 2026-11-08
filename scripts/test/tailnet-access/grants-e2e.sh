#!/bin/sh
# On-demand end-to-end test of tailnet grants through a bridge (#139). `make test-tailnet-access-e2e` runs it after
# the phase 1 test (e2e-docker.sh, the off and all regression). Needs docker. Real headscale, xray, the Go agent.
#
# One bridge "bridge-g" in tailnet_access "grants" mode. It runs the main tailscaled (tag:ingress) and one more
# tailscaled per grant group, each with the tag tag:ingress-g-<8 hex> of its group. Three tailnet hosts run a web
# server that answers with the caller's address: A (tag:nas), B (tag:printer) and C (tag:other, no target names it).
# Targets: nas -> tag:nas:8000, printer -> tag:printer:8000. Devices:
#   d1 granted nas, d2 granted printer, d3 has both through an owner grant, d4 has no grant.
# Matrix, on VLESS Reality (mihomo, as in e2e-docker.sh) and Hysteria 2 (sing-box): each device to A, B, C and a private LAN address,
#   before the ACL is applied (all denied), and after node-control writes the group rules and an admin confirms
#   them (only the granted pairs answer). Then the grant of d1 is revoked: the next sync denies it, and the ACL
#   preview shows the rule removal.
#   E2E_* image variables: as in e2e-docker.sh. E2E_KEEP=1 keeps the containers.
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
here="$root/scripts/test/tailnet-access"
hyd="$root/scripts/test/hysteria"
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
sb_image=${E2E_SINGBOX_IMAGE:-ghcr.io/sagernet/sing-box:v1.14.4}
mh_image=${E2E_MIHOMO_IMAGE:-metacubex/mihomo:v1.19.33}
xray_version=${E2E_XRAY_VERSION:-26.10.31}
front=front.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v openssl >/dev/null 2>&1 || skip "openssl is not available"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 180 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
pub="11.$oct.9"; lan="172.30.$oct"
net="tng-e2e-$sfx"; lnet="tng-e2e-lan-$sfx"
hs="tng-e2e-hs-$sfx"; nc="tng-e2e-nc-$sfx"; adm="tng-e2e-admin-$sfx"; web="tng-e2e-web-$sfx"; lsv="tng-e2e-lansrv-$sfx"
fr="tng-e2e-front-$sfx"; br="tng-e2e-bridge-$sfx"
WEB_IP="$pub.80"; LAN_IP="$lan.80"; BR_IP="$pub.23"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: networks $net $lnet, containers tng-e2e-*-$sfx, files $T"; return; fi
  docker ps -aq --filter "name=tng-e2e-.*-$sfx" | xargs -r docker rm -f >/dev/null 2>&1
  for n in "$net" "$lnet"; do dk network rm "$n" >/dev/null 2>&1; done; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() {
  echo "FATAL: $1"
  for c in "$hs" "$nc" "$br"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done
  exit 1
}
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }
gid() { printf '%s' "$1" | tr ' ' '\n' | sort | python3 -c 'import hashlib,sys;print("g-"+hashlib.sha256(sys.stdin.read().strip().encode()).hexdigest()[:8])'; }
G_NAS=$(gid nas); G_PRN=$(gid printer); G_BOTH=$(gid "nas printer")

echo "# images"
tmo 900 docker build -q -t tng-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
mkdir -p "$T/bctx/agent"
cp -R "$root/apps/ingress-agent/go.mod" "$root/apps/ingress-agent/cmd" "$root/apps/ingress-agent/internal" "$T/bctx/agent/"
cp "$hyd/Dockerfile.bridge" "$T/bctx/"; cp "$here/grants-bridge.sh" "$T/bctx/bridge.sh"
tmo 900 docker build -q --build-arg XRAY_VERSION="$xray_version" --build-arg TAILSCALE_IMAGE="$ts_image" -f "$T/bctx/Dockerfile.bridge" \
  -t tng-e2e-bridge "$T/bctx" >/dev/null || die "bridge image"
for i in "$hs_image" "$ts_image" "$sb_image" "$mh_image" python:3.13-alpine caddy:2-alpine curlimages/curl:8.17.0; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done

echo "# keys and certificates (all made here, for this run only)"
mkdir -p "$T/etc" "$T/tls" "$T/xray" "$T/nc" "$T/cl"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e test CA" \
  -keyout "$T/tls/ca.key" -out "$T/tls/ca.pem" >/dev/null 2>&1 || die "ca"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=bridge" -keyout "$T/tls/key.pem" -out "$T/tls/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=DNS:bridge-g.e2e.example\n' > "$T/tls/ext"
openssl x509 -req -in "$T/tls/csr.pem" -CA "$T/tls/ca.pem" -CAkey "$T/tls/ca.key" -CAcreateserial -days 1 \
  -extfile "$T/tls/ext" -out "$T/tls/cert.pem" >/dev/null 2>&1 || die "leaf"
chmod 644 "$T/tls/"*
kp=$(docker run --rm --entrypoint xray tng-e2e-bridge x25519) || die "x25519"
priv=$(echo "$kp" | sed -n 's/^Private[ _]*[Kk]ey: *//p')
pubk=$(echo "$kp" | sed -n -e 's/^Public[ _]*[Kk]ey: *//p' -e 's/^Password[^:]*: *//p' | head -1)
[ -n "$priv" ] && [ -n "$pubk" ] || die "reality keys"
sid=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
obfs=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')

echo "# networks"
dk network create --subnet "$pub.0/24" "$net" >/dev/null || die "network $pub.0/24"
dk network create --internal --subnet "$lan.0/24" "$lnet" >/dev/null || die "network $lan.0/24"
echoer='import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s):
        b = s.client_address[0].encode(); s.send_response(200); s.send_header("Content-Length", str(len(b))); s.end_headers(); s.wfile.write(b)
    def log_message(s, *a): pass
h.ThreadingHTTPServer(("0.0.0.0", 8000), E).serve_forever()'
dk run -d --name "$web" --network "$net" --ip "$WEB_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die web
dk run -d --name "$lsv" --network "$lnet" --ip "$LAN_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die lan
printf '%s {\n\ttls internal\n\trespond "front"\n}\n' "$front" > "$T/Caddyfile"
dk run -d --name "$fr" --network "$net" -v "$T/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null || die front

echo "# headscale, with its policy in the database"
cat > "$T/etc/config.yaml" <<YAML
server_url: http://$hs:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: true
noise:
  private_key_path: /data/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: true
    region_id: 999
    region_code: e2e
    region_name: e2e
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /data/derp_server_private.key
    automatically_add_embedded_derp_region: true
  urls: []
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /data/db.sqlite
dns:
  magic_dns: false
  override_local_dns: false
policy:
  mode: database
unix_socket: /data/headscale.sock
log:
  level: info
YAML
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec -i "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
# No rule for any bridge tag: the group identities reach nothing until node-control adds their rules. The group
# tags are in tagOwners so the keys can carry them; node-control keeps them in step with tag:ingress.
printf '%s' "{\"tagOwners\":{\"tag:node-control\":[\"e2e@\"],\"tag:ingress\":[\"e2e@\"],\"tag:nas\":[\"e2e@\"],\"tag:printer\":[\"e2e@\"],\"tag:other\":[\"e2e@\"],\"tag:ingress-$G_NAS\":[\"e2e@\"],\"tag:ingress-$G_PRN\":[\"e2e@\"],\"tag:ingress-$G_BOTH\":[\"e2e@\"]},\"acls\":[{\"action\":\"accept\",\"src\":[\"e2e@\",\"tag:node-control\"],\"dst\":[\"*:*\"]}]}" \
  | hsx policy set -f /dev/stdin >/dev/null || die "bootstrap policy"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_br=$(key tag:ingress); k_a=$(key tag:nas); k_b=$(key tag:printer); k_c=$(key tag:other)
k_g1=$(key "tag:ingress-$G_NAS"); k_g2=$(key "tag:ingress-$G_PRN"); k_g3=$(key "tag:ingress-$G_BOTH")
k_ad=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
for k in "$k_nc" "$k_br" "$k_a" "$k_b" "$k_c" "$k_g1" "$k_g2" "$k_g3" "$k_ad" "$api"; do [ -n "$k" ] || die "keys"; done
joined() { [ -n "$(docker exec "$hs" headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o "\"given_name\": *\"$1\"")" ]; }
nodeip() { hsx nodes list -o json | python3 -c "import json,sys;print([n['ip_addresses'][0] for n in json.load(sys.stdin) if n.get('given_name')=='$1'][0])" 2>/dev/null; }
ok "headscale up, policy in the database"

echo "# tailnet hosts A (tag:nas), B (tag:printer), C (tag:other), each with a web server on 8000"
host() { # container name key
  dk run -d --name "$1" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$3" \
    -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME="$2" -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start $2"
  dk run -d --name "$1-web" --network "container:$1" python:3.13-alpine python3 -c "$echoer" >/dev/null || die "$2 web"
  wait_for 60 joined "$2" || die "$2 did not join"
}
host "tng-e2e-a-$sfx" host-a "$k_a"; host "tng-e2e-b-$sfx" host-b "$k_b"; host "tng-e2e-c-$sfx" host-c "$k_c"
A_TS=$(nodeip host-a); B_TS=$(nodeip host-b); C_TS=$(nodeip host-c)

echo "# the grant bridge: main tailscaled, three group tailscaleds, xray (VLESS Reality and Hysteria 2), the agent"
python3 - "$T/xray/config.base.json" "$fr" "$front" "$priv" "$sid" "$obfs" "$G_NAS" "$G_PRN" "$G_BOTH" <<'PY'
import json, sys
out, fr, front, priv, sid, obfs = sys.argv[1:7]
groups = sys.argv[7:]
socks = lambda tag, port: {"tag": tag, "protocol": "socks", "settings": {"servers": [{"address": "127.0.0.1", "port": port}]}}
cfg = {
  "log": {"loglevel": "warning", "access": "none"}, "stats": {}, "api": {"tag": "api", "services": ["StatsService"]},
  "policy": {"levels": {"0": {"statsUserUplink": True, "statsUserDownlink": True}}},
  "inbounds": [
    {"tag": "reality-in", "listen": "0.0.0.0", "port": 443, "protocol": "vless", "settings": {"clients": [], "decryption": "none"},
     "streamSettings": {"network": "tcp", "security": "reality",
       "realitySettings": {"dest": fr + ":443", "serverNames": [front], "privateKey": priv, "shortIds": [sid]}},
     "sniffing": {"enabled": True, "destOverride": ["http", "tls"], "routeOnly": True}},
    {"tag": "hy2-in", "listen": "0.0.0.0", "port": 443, "protocol": "hysteria", "settings": {"version": 2, "clients": []},
     "streamSettings": {"network": "hysteria", "hysteriaSettings": {"version": 2}, "security": "tls",
       "tlsSettings": {"alpn": ["h3"], "certificates": [{"certificateFile": "/etc/xray/tls/cert.pem", "keyFile": "/etc/xray/tls/key.pem"}]},
       "finalmask": {"udp": [{"type": "salamander", "settings": {"password": obfs}}]}},
     "sniffing": {"enabled": True, "destOverride": ["http", "tls", "quic"], "routeOnly": True}},
    {"tag": "api", "listen": "127.0.0.1", "port": 10085, "protocol": "dokodemo-door", "settings": {"address": "127.0.0.1"}}],
  "outbounds": [{"tag": "blocked", "protocol": "blackhole"},
    {"tag": "direct", "protocol": "freedom", "streamSettings": {"sockopt": {"domainStrategy": "UseIPv4"}}},
    socks("via-exit", 1055), socks("via-tailnet", 1055)] + [socks("via-tailnet-" + g, 1056 + i) for i, g in enumerate(groups)],
  "routing": {"domainStrategy": "IPOnDemand", "rules": [{"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
    {"type": "field", "network": "tcp,udp", "outboundTag": "blocked"}]}}
json.dump(cfg, open(out, "w"))
PY
chmod 644 "$T/xray/config.base.json"
dk run -d --name "$br" --network "$net" --ip "$BR_IP" \
  -v "$T/xray/config.base.json:/etc/xray/config.base.json:ro" -v "$T/tls:/etc/xray/tls:ro" \
  -e LOGIN_SERVER="http://$hs:8080" -e TS_AUTHKEY="$k_br" -e TS_HOSTNAME=bridge-g -e AGENT_TOKEN="$token" \
  -e GROUP_KEYS="$G_NAS=$k_g1 $G_PRN=$k_g2 $G_BOTH=$k_g3" \
  -e INGRESS_NAME=bridge-g -e INGRESS_HOST=bridge-g.e2e.example -e INGRESS_PUBLIC_KEY="$pubk" -e INGRESS_SHORT_ID="$sid" \
  -e INGRESS_SNI="$front" -e INGRESS_HY2_PORT=443 tng-e2e-bridge >/dev/null || die "start bridge"
dk network connect --ip "$lan.23" "$lnet" "$br" >/dev/null || die "bridge to the lan"
wait_for 90 joined "bridge-g-$G_BOTH" || die "the group nodes did not join"
BR_TS=$(nodeip bridge-g)
docker exec "$br" curl -fsS --max-time 5 "http://$LAN_IP:8000/" >/dev/null 2>&1 \
  && ok "control: the bridge itself reaches the private LAN server" || die "the bridge does not reach the LAN (control)"
G1_TS=$(nodeip "bridge-g-$G_NAS"); G2_TS=$(nodeip "bridge-g-$G_PRN"); G3_TS=$(nodeip "bridge-g-$G_BOTH")

echo "# node-control: bridge-g in grants mode, targets nas and printer, the policy console"
cat > "$T/nc/enroll.json" <<JSON
{"profile_name": "tunnel", "link_host": "links.e2e.example",
 "tailnet_targets": [{"name": "nas", "dst": ["tag:nas:8000"]}, {"name": "printer", "dst": ["tag:printer:8000"]}],
 "ingresses": [
  {"name": "bridge-g", "host": "bridge-g.e2e.example", "public_key": "$pubk", "short_id": "$sid", "sni": "$front",
   "port": 443, "hy2_port": 443, "agent_url": "http://$BR_TS:9000", "token_env": "BRIDGE_TOKEN", "uuids": "base",
   "tailnet_access": "grants"}]}
JSON
chmod 644 "$T/nc/enroll.json"
dk run -d --name "$nc" --network "$net" -v "$T/nc:/etc/nc:ro" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test -e POLICY_ADMIN_USERS=e2e@example.test \
  -e POLICY_DB=/tmp/policy.db -e ENROLL_ALLOWED_HOSTS=node-control \
  -e ENROLL_CONFIG=/etc/nc/enroll.json -e ENROLL_DB=/tmp/enroll.db -e ENROLL_SECRET_KEY="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')" \
  -e BRIDGE_TOKEN="$token" -e HY2_OBFS_BRIDGE_G="$obfs" tng-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 joined node-control || die "node-control did not join"
NC_TS=$(nodeip node-control)

echo "# an admin enrols four devices and gives the grants"
dk run -d --name "$adm" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$k_ad" \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=admin -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start admin"
dk exec "$adm" apk add --no-cache -q curl >/dev/null 2>&1 || die "admin tools"
dk exec "$adm" sh -c "echo '$NC_TS node-control' >> /etc/hosts" || die "admin hosts"
A="http://node-control"
acurl() { docker exec "$adm" curl -sS --max-time 30 "$@"; }
apost() { acurl -X POST -H 'Content-Type: application/json' -d "$2" "$A$1"; }
wait_for 60 docker exec "$adm" curl -fsS --max-time 5 "$A/enroll/devices" || die "admin listener: $(docker logs --tail 5 "$nc" 2>&1)"
exp=$(( $(date +%s) + 86400 ))
for n in 1 2 3 4; do
  case $n in 1) o=alice ;; 2) o=bob ;; 3) o=carol ;; 4) o=dave ;; esac
  r=$(apost /enroll/devices "{\"name\":\"d$n\",\"owner\":\"$o\",\"client\":\"sing-box\",\"expires_at\":$exp}") || die "create d$n"
  id=$(echo "$r" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); s=$(echo "$r" | sed -n 's#.*"subscription_url":"https://[^/]*/sub/\([^"]*\)".*#\1#p')
  [ -n "$id" ] && [ -n "$s" ] || die "device d$n: $r"
  eval "ID$n=$id"
  acurl -f -A e2e-client "http://$NC_TS:8081/sub/$s" > "$T/cl/d$n.json" || die "subscription d$n"
done
ok "the admin enrolled four sing-box devices"
grant() { apost /enroll/tailnet-grants/subject "{\"bridge\":\"bridge-g\",\"kind\":\"$1\",\"subject\":\"$2\",\"targets\":$3}"; }
grant device "$ID1" '["nas"]' | grep -q '"ok":true' && ok "d1 granted nas" || nok "grant d1"
grant device "$ID2" '["printer"]' | grep -q '"ok":true' && ok "d2 granted printer" || nok "grant d2"
grant owner carol '["nas","printer"]' | grep -q '"ok":true' && ok "owner carol (d3) granted nas and printer" || nok "grant carol"

grants() { docker exec "$br" python3 -c "
import json; c = json.load(open('/etc/xray/config.json'))
print(' '.join(sorted(r['outboundTag'] for r in c['routing']['rules'] if r.get('outboundTag', '').startswith('via-tailnet'))))" 2>/dev/null; }
want_rules="via-tailnet-$G_BOTH via-tailnet-$G_NAS via-tailnet-$G_PRN"
want_rules=$(echo "$want_rules" | tr ' ' '\n' | sort | tr '\n' ' ' | sed 's/ $//')
wait_for 60 sh -c "[ \"\$(docker exec $br python3 -c \"import json;c=json.load(open('/etc/xray/config.json'));print(len([r for r in c['routing']['rules'] if r.get('outboundTag','').startswith('via-tailnet-')]))\")\" = 3 ]" \
  && ok "the bridge has one tailnet rule per grant group: $(grants)" || nok "group rules on the bridge: '$(grants)', want '$want_rules'"
st=$(acurl "$A/enroll/tailnet-grants")
[ "$(echo "$st" | grep -o '"status":"ok"\|"status":"pending_acl"' | wc -l | tr -d ' ')" = 3 ] \
  && ok "the console sees the three groups as provisioned" || nok "group status: $st"

pin() { # device protocol -> a sing-box config with a local mixed port and the bridge outbound of that protocol
  python3 - "$T/cl/d$1.json" "$2" > "$T/cl/d$1-$2.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["inbounds"] = [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": 2080}]
p["route"]["final"] = "direct-bridge-g" + ("-hy2" if sys.argv[2] == "hy2" else "")
p.pop("dns", None); p["route"].pop("default_domain_resolver", None)
p["route"]["rules"] = [r for r in p["route"]["rules"] if r.get("action") != "hijack-dns"]
print(json.dumps(p))
PY
}
# mh: the VLESS Reality outbound of the device's sing-box profile, as one mihomo proxy (the same credential).
mh() { python3 - "$T/cl/d$1.json" "$BR_IP" > "$T/cl/mh-d$1/config.yaml" <<'PY'
import json, sys
o = [x for x in json.load(open(sys.argv[1]))["outbounds"] if x.get("tag") == "direct-bridge-g"][0]
t = o["tls"]
p = {"name": "b", "type": "vless", "server": o["server"], "port": o["server_port"], "uuid": o["uuid"], "network": "tcp",
     "tls": True, "udp": True, "flow": o.get("flow", "xtls-rprx-vision"), "servername": t["server_name"],
     "client-fingerprint": "chrome", "reality-opts": {"public-key": t["reality"]["public_key"], "short-id": t["reality"]["short_id"],
     "support-x25519mlkem768": True}}
print(json.dumps({"mixed-port": 2080, "bind-address": "127.0.0.1", "allow-lan": False, "proxies": [p], "rules": ["MATCH,b"],
  "hosts": {o["server"]: sys.argv[2]},
  "dns": {"enable": True, "use-hosts": True, "nameserver": ["system"], "proxy-server-nameserver": ["system"]}}))
PY
}
client() { # device protocol -> container name
  c="tng-e2e-cl-d$1-$2-$sfx"; docker rm -f "$c" >/dev/null 2>&1
  if [ "$2" = vless ]; then
    mkdir -p "$T/cl/mh-d$1"; mh "$1"
    dk run -d --name "$c" --network "$net" --add-host "bridge-g.e2e.example:$BR_IP" \
      -v "$T/cl/mh-d$1:/conf" "$mh_image" -d /conf >/dev/null || return 1
  else
    pin "$1" "$2"
    dk run -d --name "$c" --network "$net" --add-host "bridge-g.e2e.example:$BR_IP" -e SSL_CERT_FILE=/ca.pem \
      -v "$T/tls/ca.pem:/ca.pem:ro" -v "$T/cl/d$1-$2.json:/c.json:ro" "$sb_image" run -c /c.json >/dev/null || return 1
  fi
  echo "$c"
}
via() { docker run --rm --network "container:$1" curlimages/curl:8.17.0 -fsS --max-time 8 --socks5-hostname 127.0.0.1:2080 "$2" 2>/dev/null; }
up() { i=0; while [ $i -lt 15 ]; do via "$1" "http://$WEB_IP:8000/" >/dev/null && return 0; i=$((i + 1)); sleep 1; done; return 1; }
# probe container url -> the caller address the host saw, or "-" when refused (two tries for a granted path)
probe() { got=""; i=0; while [ $i -lt "$3" ]; do got=$(via "$1" "$2") && { echo "$got"; return; }; i=$((i + 1)); sleep 1; done; echo "-"; }
MATRIX="$T/matrix"; : > "$MATRIX"
# check phase device protocol host want(caller ts ip or -)
check() {
  case $4 in A) u="http://$A_TS:8000/" ;; B) u="http://$B_TS:8000/" ;; C) u="http://$C_TS:8000/" ;; LAN) u="http://$LAN_IP:8000/" ;; esac
  tries=1; [ "$5" != - ] && tries=10
  got=$(probe "$CL" "$u" "$tries")
  cell=denied; [ "$got" != - ] && cell=allowed
  echo "$1 d$2 $3 $4 $cell" >> "$MATRIX"
  if [ "$got" = "$5" ]; then ok "$1: d$2 over $3 to $4 is $cell${got#-}"; else nok "$1: d$2 over $3 to $4: want '$5', got '$got'"; fi
}
run_matrix() { # phase, then "device:A-want:B-want:C-want" items; LAN is always denied
  phase=$1; shift
  for proto in vless hy2; do
    for spec in "$@"; do
      d=${spec%%:*}; rest=${spec#*:}; wa=${rest%%:*}; rest=${rest#*:}; wb=${rest%%:*}; wc=${rest#*:}
      CL=$(client "$d" "$proto") || { nok "$phase: d$d $proto client start"; continue; }
      up "$CL" || nok "$phase: d$d over $proto: no public path"
      check "$phase" "$d" "$proto" A "$wa"; check "$phase" "$d" "$proto" B "$wb"; check "$phase" "$d" "$proto" C "$wc"
      check "$phase" "$d" "$proto" LAN -
      docker rm -f "$CL" >/dev/null 2>&1
    done
  done
}

echo "# before the ACL is applied: every pair is denied"
run_matrix before "1:-:-:-" "2:-:-:-" "3:-:-:-" "4:-:-:-"

echo "# node-control writes the group rules as one policy change, and the admin confirms it"
pre=$(acurl "$A/enroll/tailnet-acl")
[ "$(echo "$pre" | grep -o '"op":"add"' | wc -l | tr -d ' ')" = 3 ] && ok "the preview adds three group rules" || nok "preview: $pre"
res=$(apost /enroll/tailnet-acl '{}'); cid=$(echo "$res" | sed -n 's/.*"change_id":\([0-9]*\).*/\1/p')
[ -n "$cid" ] && ok "node-control submitted policy change $cid" || die "apply: $res"
code=$(acurl -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"change_id\":$cid}" "$A/policy/confirm")
[ "$code" = 204 ] && ok "the admin confirmed the change" || die "confirm: $code"
acurl "$A/enroll/tailnet-acl" | grep -q '"status":"in_sync"' && ok "the console reports the rules in sync" || nok "not in sync"
sleep 5

echo "# after the ACL: only the granted pairs answer, each through its own group identity"
run_matrix after "1:$G1_TS:-:-" "2:-:$G2_TS:-" "3:$G3_TS:$G3_TS:-" "4:-:-:-"

echo "# revoke the grant of d1"
grant device "$ID1" '[]' | grep -q '"ok":true' && ok "the grant of d1 is revoked" || nok "revoke d1"
ch=$(acurl "$A/enroll/tailnet-acl")
echo "$ch" | grep -q "\"op\":\"remove\",\"src\":\"tag:ingress-$G_NAS\"" && ok "the preview shows the removal of the nas group rule" || nok "preview after revoke: $ch"
run_matrix revoked "1:-:-:-"

echo
echo "| Phase | Device | Protocol | A (nas) | B (printer) | C (other) | LAN |"
echo "|---|---|---|---|---|---|---|"
python3 - "$MATRIX" <<'PY'
import sys
rows = {}
for line in open(sys.argv[1]):
    ph, d, pr, host, cell = line.split()
    rows.setdefault((ph, d, pr), {})[host] = cell
for (ph, d, pr), c in rows.items():
    print(f"| {ph} | {d} | {'VLESS' if pr == 'vless' else 'Hysteria 2'} | {c.get('A','')} | {c.get('B','')} | {c.get('C','')} | {c.get('LAN','')} |")
PY
echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

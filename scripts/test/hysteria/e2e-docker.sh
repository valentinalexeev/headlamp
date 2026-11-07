#!/bin/sh
# On-demand end-to-end spike of Hysteria 2 beside VLESS Reality (#110). Run it with `make test-hysteria-e2e`.
# Needs docker. It builds node-control and a bridge test image (xray, the ingress agent, tailscaled) from this tree.
#
# Three docker networks. "pub" stands for the internet near the bridge: headscale, node-control, the bridge, the
# clients and a web server that answers with the caller's address. "far" is the internet that only the exit node
# reaches, with a second such web server. Both use addresses outside geoip:private, so the bridge routes them.
# "lan" is private, with a server that only the bridge can reach.
#
# Flow: the bridge joins the tailnet. Its agent gets the credentials from node-control. An admin on the tailnet
# enrols a device, and the test fetches the device's sing-box and Clash profiles.
# Real sing-box and mihomo clients then use each generated outbound: VLESS and Hysteria 2, in each mode.
#   direct    -> the pub web server sees the bridge, and the far one is out of reach.
#   exit/auto -> the far web server sees the exit node.
#   Private and tailnet destinations are refused on both protocols.
#   E2E_HEADSCALE_IMAGE  default jauderho/headscale:v0.29.3
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.98.10
#   E2E_SINGBOX_IMAGE    default ghcr.io/sagernet/sing-box:v1.14.4
#   E2E_MIHOMO_IMAGE     default metacubex/mihomo:v1.19.33
#   E2E_XRAY_VERSION     default 26.10.31
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
here="$root/scripts/test/hysteria"
hs_image=${E2E_HEADSCALE_IMAGE:-jauderho/headscale:v0.29.3}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.98.10}
sb_image=${E2E_SINGBOX_IMAGE:-ghcr.io/sagernet/sing-box:v1.14.4}
mh_image=${E2E_MIHOMO_IMAGE:-metacubex/mihomo:v1.19.33}
xray_version=${E2E_XRAY_VERSION:-26.10.31}
host=bridge.e2e.example
front=front.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v openssl >/dev/null 2>&1 || skip "openssl is not available"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 180 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
# 11.0.0.0/8 is not in geoip:private, so to the bridge this network is "the internet".
pub="11.$oct.7"; far="12.$oct.9"; lan="172.30.$oct"
net="hy2-e2e-$sfx"; fnet="hy2-e2e-far-$sfx"; lnet="hy2-e2e-lan-$sfx"
hs="hy2-e2e-hs-$sfx"; nc="hy2-e2e-nc-$sfx"; ex="hy2-e2e-exit-$sfx"; br="hy2-e2e-bridge-$sfx"; web="hy2-e2e-web-$sfx"
lsv="hy2-e2e-lansrv-$sfx"; fr="hy2-e2e-front-$sfx"; adm="hy2-e2e-admin-$sfx"
WEB_IP="$pub.80"; BR_IP="$pub.20"; FAR_IP="$far.80"; EX_FAR="$far.30"; LAN_IP="$lan.80"; fweb="hy2-e2e-farweb-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: networks $net $fnet $lnet, containers hy2-e2e-*-$sfx, files $T"; return; fi
  docker ps -aq --filter "name=hy2-e2e-.*-$sfx" | xargs -r docker rm -f >/dev/null 2>&1
  for n in "$net" "$fnet" "$lnet"; do dk network rm "$n" >/dev/null 2>&1; done; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() {
  echo "FATAL: $1"
  for c in "$hs" "$nc" "$ex" "$br" "$adm"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done
  docker exec "$br" sh -c 'tail -n 20 /var/log/xray.log' 2>/dev/null; exit 1
}
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# images"
dk build -q -t hy2-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
mkdir -p "$T/bctx"
cp -R "$root/apps/ingress-agent/app" "$root/apps/ingress-agent/requirements.txt" "$T/bctx/"
cp "$here/Dockerfile.bridge" "$here/bridge.sh" "$T/bctx/"
dk build -q --build-arg XRAY_VERSION="$xray_version" --build-arg TAILSCALE_IMAGE="$ts_image" -f "$T/bctx/Dockerfile.bridge" \
  -t hy2-e2e-bridge "$T/bctx" >/dev/null || die "bridge image"
for i in "$hs_image" "$ts_image" "$sb_image" "$mh_image" python:3.13-alpine caddy:2-alpine curlimages/curl:8.17.0; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done

echo "# keys and certificates (all made here, for this run only)"
mkdir -p "$T/etc" "$T/tls" "$T/xray" "$T/nc" "$T/cl"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e test CA" \
  -keyout "$T/tls/ca.key" -out "$T/tls/ca.pem" >/dev/null 2>&1 || die "ca"
# The Hysteria 2 certificate: on a real bridge it comes from ACME HTTP-01 (#110); here a test CA signs it.
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$host" -keyout "$T/tls/key.pem" -out "$T/tls/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s\n' "$host" > "$T/tls/ext"
openssl x509 -req -in "$T/tls/csr.pem" -CA "$T/tls/ca.pem" -CAkey "$T/tls/ca.key" -CAcreateserial -days 1 \
  -extfile "$T/tls/ext" -out "$T/tls/cert.pem" >/dev/null 2>&1 || die "leaf"
chmod 644 "$T/tls/"*
kp=$(docker run --rm --entrypoint xray hy2-e2e-bridge x25519) || die "x25519"
priv=$(echo "$kp" | sed -n 's/^Private[ _]*[Kk]ey: *//p')
# newer xray prints "Password (PublicKey): ...", older "Public key: ..."
pubk=$(echo "$kp" | sed -n -e 's/^Public[ _]*[Kk]ey: *//p' -e 's/^Password[^:]*: *//p' | head -1)
[ -n "$priv" ] && [ -n "$pubk" ] || die "reality keys"
sid=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
obfs=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')

echo "# networks"
dk network create --subnet "$pub.0/24" "$net" >/dev/null || die "network $pub.0/24"
dk network create --internal --subnet "$far.0/24" "$fnet" >/dev/null || die "network $far.0/24"
dk network create --internal --subnet "$lan.0/24" "$lnet" >/dev/null || die "network $lan.0/24"
echoer='import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s):
        b = s.client_address[0].encode(); s.send_response(200); s.send_header("Content-Length", str(len(b))); s.end_headers(); s.wfile.write(b)
    def log_message(s, *a): pass
h.ThreadingHTTPServer(("0.0.0.0", 8000), E).serve_forever()'
dk run -d --name "$web" --network "$net" --ip "$WEB_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die web
dk run -d --name "$fweb" --network "$fnet" --ip "$FAR_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die farweb
dk run -d --name "$lsv" --network "$lnet" --ip "$LAN_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die lan
# The Reality target: a TLS 1.3 site for the front name, that unauthenticated probes see.
printf '%s {\n\ttls internal\n\trespond "front"\n}\n' "$front" > "$T/Caddyfile"
dk run -d --name "$fr" --network "$net" -v "$T/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null || die front

echo "# headscale"
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
  magic_dns: true
  base_domain: tailnet.e2e.test
  override_local_dns: false
  nameservers:
    global: []
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /data/headscale.sock
log:
  level: info
YAML
cat > "$T/etc/acl.json" <<'JSON'
{
  "tagOwners": {"tag:node-control": ["e2e@"], "tag:ingress": ["e2e@"], "tag:exit": ["e2e@"]},
  "autoApprovers": {"exitNode": ["tag:exit"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON
dk run -d --name "$hs" --network "$net" --entrypoint "" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  sh -c 'mkdir -p /data && exec headscale serve -c /etc/headscale/config.yaml' >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_ex=$(key tag:exit); k_br=$(key tag:ingress); k_ad=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_ex" ] && [ -n "$k_br" ] && [ -n "$k_ad" ] && [ -n "$api" ] || die "keys"
nodeip() { hsx nodes list -o json | python3 -c "import json,sys;print([n['ip_addresses'][0] for n in json.load(sys.stdin) if n.get('given_name')=='$1'][0])" 2>/dev/null; }
ok "headscale up"

echo "# exit node (on pub for the tailnet, and the only node on far)"
dk run -d --name "$ex" --network "$net" -e TS_AUTHKEY="$k_ex" -e TS_HOSTNAME=exit -e TS_USERSPACE=true \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --advertise-exit-node" "$ts_image" >/dev/null || die "start exit"
dk network connect --ip "$EX_FAR" "$fnet" "$ex" >/dev/null || die "exit to far"
# An exit node does not forward to a network it is attached to unless it also advertises that network. A real exit
# reaches the internet through a gateway; here "far" is attached, so the exit advertises it as a route as well.
wait_for 60 sh -c "[ -n \"\$(docker exec $hs headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o '\"given_name\": *\"exit\"')\" ]" || die "exit did not join"
EXIT_TS=$(nodeip exit)
exid=$(hsx nodes list -o json | python3 -c "import json,sys;print([n['id'] for n in json.load(sys.stdin) if n.get('given_name')=='exit'][0])")
dk exec "$ex" tailscale set --advertise-exit-node --advertise-routes="$far.0/24" >/dev/null || die "exit routes"
wait_for 20 hsx nodes approve-routes -i "$exid" -r "0.0.0.0/0,::/0,$far.0/24" || die "approve exit routes"

echo "# bridge: xray with VLESS Reality on 443/tcp and Hysteria 2 on 443/udp, the agent, tailscaled"
cat > "$T/xray/config.base.json" <<JSON
{
  "log": {"loglevel": "warning", "access": "none"},
  "stats": {}, "api": {"tag": "api", "services": ["StatsService"]},
  "policy": {"levels": {"0": {"statsUserUplink": true, "statsUserDownlink": true}}},
  "inbounds": [
    {"tag": "reality-in", "listen": "0.0.0.0", "port": 443, "protocol": "vless",
     "settings": {"clients": [], "decryption": "none"},
     "streamSettings": {"network": "tcp", "security": "reality",
       "realitySettings": {"dest": "$fr:443", "serverNames": ["$front"], "privateKey": "$priv", "shortIds": ["$sid"]}},
     "sniffing": {"enabled": true, "destOverride": ["http", "tls"], "routeOnly": true}},
    {"tag": "hy2-in", "listen": "0.0.0.0", "port": 443, "protocol": "hysteria",
     "settings": {"version": 2, "clients": []},
     "streamSettings": {"network": "hysteria", "hysteriaSettings": {"version": 2},
       "security": "tls", "tlsSettings": {"alpn": ["h3"],
         "certificates": [{"certificateFile": "/etc/xray/tls/cert.pem", "keyFile": "/etc/xray/tls/key.pem"}]},
       "finalmask": {"udp": [{"type": "salamander", "settings": {"password": "$obfs"}}]}},
     "sniffing": {"enabled": true, "destOverride": ["http", "tls", "quic"], "routeOnly": true}},
    {"tag": "api", "listen": "127.0.0.1", "port": 10085, "protocol": "dokodemo-door", "settings": {"address": "127.0.0.1"}}
  ],
  "outbounds": [
    {"tag": "blocked", "protocol": "blackhole"},
    {"tag": "direct", "protocol": "freedom", "streamSettings": {"sockopt": {"domainStrategy": "UseIPv4"}}},
    {"tag": "via-exit", "protocol": "socks", "settings": {"servers": [{"address": "127.0.0.1", "port": 1055}]}}
  ],
  "routing": {"domainStrategy": "IPOnDemand", "rules": [
    {"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
    {"type": "field", "network": "tcp,udp", "outboundTag": "blocked"}
  ]}
}
JSON
chmod 644 "$T/xray/config.base.json"
dk run -d --name "$br" --network "$net" --ip "$BR_IP" \
  -v "$T/xray/config.base.json:/etc/xray/config.base.json:ro" -v "$T/tls:/etc/xray/tls:ro" \
  -e LOGIN_SERVER="http://$hs:8080" -e TS_AUTHKEY="$k_br" -e AGENT_TOKEN="$token" \
  -e INGRESS_NAME=bridge-1 -e INGRESS_HOST="$host" -e INGRESS_PUBLIC_KEY="$pubk" -e INGRESS_SHORT_ID="$sid" \
  -e INGRESS_SNI="$front" -e INGRESS_HY2_PORT=443 hy2-e2e-bridge >/dev/null || die "start bridge"
dk network connect --ip "$lan.20" "$lnet" "$br" >/dev/null || die "bridge to the lan"
wait_for 60 sh -c "[ -n \"\$(docker exec $hs headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o '\"given_name\": *\"bridge\"')\" ]" || die "bridge did not join"
BR_TS=$(nodeip bridge)
wait_for 30 docker exec "$br" tailscale set --exit-node="$EXIT_TS" || die "the bridge cannot use the exit node"
wait_for 30 docker exec "$br" sh -c "curl -fsS --max-time 5 --socks5 127.0.0.1:1055 http://$FAR_IP:8000/ | grep -qx $EX_FAR" \
  && ok "the bridge's SOCKS5 leaves through the exit node" || die "exit path from the bridge"
docker exec "$br" sh -c "curl -fsS --max-time 5 http://$LAN_IP:8000/" >/dev/null 2>&1 \
  && ok "control: the bridge itself reaches the private LAN server" || die "the bridge does not reach the LAN (control)"
wait_for 30 docker exec "$br" sh -c "curl -fsS --max-time 3 -H 'Authorization: Bearer $token' http://127.0.0.1:9000/v1/info | grep -q hy2_port" \
  && ok "the agent publishes hy2_port in /v1/info" || die "agent info"

echo "# node-control with enrolment; the bridge is configured with its agent on the tailnet"
cat > "$T/nc/enroll.json" <<JSON
{"profile_name": "tunnel", "link_host": "links.e2e.example", "ingresses": [
  {"name": "bridge-1", "host": "$host", "public_key": "$pubk", "short_id": "$sid", "sni": "$front", "port": 443,
   "hy2_port": 443, "agent_url": "http://$BR_TS:9000", "token_env": "BRIDGE_TOKEN", "uuids": "base"}]}
JSON
chmod 644 "$T/nc/enroll.json"
dk run -d --name "$nc" --network "$net" -v "$T/nc:/etc/nc:ro" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test \
  -e ENROLL_CONFIG=/etc/nc/enroll.json -e ENROLL_DB=/tmp/enroll.db -e ENROLL_SECRET_KEY="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')" \
  -e BRIDGE_TOKEN="$token" -e HY2_OBFS_BRIDGE_1="$obfs" hy2-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 sh -c "[ -n \"\$(docker exec $hs headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o '\"given_name\": *\"node-control\"')\" ]" || die "node-control did not join"
NC_TS=$(nodeip node-control)

echo "# an admin on the tailnet enrols a device"
dk run -d --name "$adm" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$k_ad" \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=admin -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start admin"
dk exec "$adm" apk add --no-cache -q curl >/dev/null 2>&1 || die "admin tools"
wait_for 60 docker exec "$adm" curl -fsS --max-time 5 "http://$NC_TS/enroll/devices" || die "admin listener"
exp=$(( $(date +%s) + 86400 ))
newdev() { docker exec "$adm" curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' \
  -d "{\"name\":\"$1\",\"owner\":\"e2e\",\"client\":\"$2\",\"expires_at\":$exp}" "http://$NC_TS/enroll/devices"; }
d1=$(newdev phone sing-box) || die "create device: $(docker logs --tail 5 "$nc" 2>&1)"
d2=$(newdev laptop clash) || die "create clash device"
s1=$(echo "$d1" | sed -n 's#.*"subscription_url":"https://[^/]*/sub/\([^"]*\)".*#\1#p')
s2=$(echo "$d2" | sed -n 's#.*"subscription_url":"https://[^/]*/sub/\([^"]*\)".*#\1#p')
[ -n "$s1" ] && [ -n "$s2" ] && ok "the admin enrolled a sing-box and a Clash device" || die "no subscription urls ($d1)"
sub() { docker exec "$adm" curl -fsS --max-time 10 -A e2e-client "http://$NC_TS:8081/sub/$1"; }
sub "$s1" > "$T/cl/singbox.json" || die "sing-box subscription"
sub "$s2" > "$T/cl/clash.json" || die "clash subscription"
python3 - "$T/cl/singbox.json" <<'PY' && ok "the sing-box profile has VLESS and Hysteria 2 for each mode, grouped" || nok "sing-box profile shape"
import json, sys
p = json.load(open(sys.argv[1])); by = {o["tag"]: o for o in p["outbounds"]}
for m in ("auto", "direct", "exit"):
    assert by[m]["type"] == "urltest" and by[m]["outbounds"] == [m + "-bridge-1", m + "-bridge-1-hy2"], by[m]
    h = by[m + "-bridge-1-hy2"]; assert h["type"] == "hysteria2" and h["obfs"]["type"] == "salamander", h
PY
# the agent applied the device credentials on both inbounds
wait_for 60 docker exec "$br" sh -c "python3 -c \"import json;c=json.load(open('/etc/xray/config.json'));i={x['tag']:x for x in c['inbounds']};assert len(i['hy2-in']['settings']['clients'])==6 and len(i['reality-in']['settings']['clients'])==6\"" \
  && ok "the agent put the six device credentials on both inbounds" || die "credentials not applied"
sleep 2

echo "# clients"
# The generated outbounds are used unchanged. The test only swaps the TUN inbound for a local SOCKS port and
# pins the route to one outbound, so each protocol and mode is checked on its own.
pin_sb() { python3 - "$T/cl/singbox.json" "$1" > "$T/cl/sb-$1.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["inbounds"] = [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": 2080}]
p["route"]["final"] = sys.argv[2]
p.pop("dns", None); p["route"].pop("default_domain_resolver", None)
p["route"]["rules"] = [r for r in p["route"]["rules"] if r.get("action") != "hijack-dns"]
print(json.dumps(p))
PY
}
pin_mh() { python3 - "$T/cl/clash.json" "$1" "$host" "$BR_IP" > "$T/cl/mh-$1.yaml" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p.update({"mixed-port": 2080, "bind-address": "127.0.0.1", "allow-lan": False, "rules": ["MATCH," + sys.argv[2]]})
# mihomo does not read /etc/hosts: the bridge name is given to it directly.
p["hosts"] = {sys.argv[3]: sys.argv[4]}
p["dns"] = {"enable": True, "use-hosts": True, "nameserver": ["system"], "proxy-server-nameserver": ["system"]}
print(json.dumps(p))
PY
}
client() { # kind tag -> starts the client, prints the container name
  c="hy2-e2e-$1-$(echo "$2" | tr -c 'a-z0-9-\n' '-')-$sfx"
  if [ "$1" = sb ]; then pin_sb "$2"; f="$T/cl/sb-$2.json"
    dk run -d --name "$c" --network "$net" --add-host "$host:$BR_IP" -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
      -v "$f:/c.json:ro" "$sb_image" run -c /c.json >/dev/null || return 1
  else pin_mh "$2"; f="$T/cl/mh-$2.yaml"
    mkdir -p "$T/cl/mh-$2.d"; cp "$f" "$T/cl/mh-$2.d/config.yaml"
    dk run -d --name "$c" --network "$net" --add-host "$host:$BR_IP" -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
      -v "$T/cl/mh-$2.d:/conf" "$mh_image" -d /conf >/dev/null || return 1
  fi
  echo "$c"
}
via() { docker run --rm --network "container:$1" curlimages/curl:8.17.0 -fsS --max-time 8 --socks5-hostname 127.0.0.1:2080 "$2" 2>/dev/null; }
check() { # kind tag url expect-from label
  c=$(client "$1" "$2") || { nok "$5: client start"; return; }
  got=""; i=0; while [ $i -lt 15 ]; do got=$(via "$c" "$3") && break; i=$((i + 1)); sleep 1; done
  if [ "$got" = "$4" ]; then ok "$5: the web server sees $4"; else nok "$5: want $4, got '${got}' ($(docker logs --tail 3 "$c" 2>&1 | tr '\n' ' '))"; fi
  if [ "$4" = "$BR_IP" ]; then
    if via "$c" "http://$FAR_IP:8000/" >/dev/null; then nok "$5: direct must not reach the far network"; else ok "$5: direct does not reach the far network"; fi
  fi
  lbl=$5
  if via "$c" "http://$LAN_IP:8000/" >/dev/null; then nok "$lbl: a private address must be refused"; else ok "$lbl: a private address is refused"; fi
  if via "$c" "http://$NC_TS:8081/" >/dev/null; then nok "$lbl: a tailnet address must be refused"; else ok "$lbl: a tailnet address is refused"; fi
  docker rm -f "$c" >/dev/null 2>&1
}
near="http://$WEB_IP:8000/"; farurl="http://$FAR_IP:8000/"
check sb direct-bridge-1-hy2 "$near"   "$BR_IP"  "sing-box Hysteria 2, direct"
check sb exit-bridge-1-hy2   "$farurl" "$EX_FAR" "sing-box Hysteria 2, exit"
check sb auto-bridge-1-hy2   "$farurl" "$EX_FAR" "sing-box Hysteria 2, auto"
check mh direct-bridge-1-hy2 "$near"   "$BR_IP"  "mihomo Hysteria 2, direct"
check mh exit-bridge-1-hy2   "$farurl" "$EX_FAR" "mihomo Hysteria 2, exit"
check mh direct-bridge-1     "$near"   "$BR_IP"  "mihomo VLESS Reality, direct"
check mh exit-bridge-1       "$farurl" "$EX_FAR" "mihomo VLESS Reality, exit"

# Known upstream problem, recorded and not counted: Xray-core 26.9.8 and later needs an X25519MLKEM768 key share
# in the Reality ClientHello, and stock sing-box (1.14.4) does not send one (SagerNet/sing-box#4520). Thus sing-box
# clients cannot use VLESS Reality on a current bridge. Hysteria 2 does not depend on it.
c=$(client sb direct-bridge-1)
sleep 3
if got=$(via "$c" "$near"); then echo "# note: sing-box VLESS Reality works again ($got); the upstream issue may be fixed"
else echo "# known: sing-box VLESS Reality is refused by xray $xray_version (SagerNet/sing-box#4520); Hysteria 2 works"; fi
docker rm -f "$c" >/dev/null 2>&1

echo "# wrong secrets and revocation"
# Without the Salamander password the server does not answer: the same outbound with obfs removed fails.
python3 - "$T/cl/singbox.json" > "$T/cl/sb-noobfs.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
for o in p["outbounds"]:
    o.pop("obfs", None)
p["inbounds"] = [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": 2080}]
p["route"]["final"] = "direct-bridge-1-hy2"; p.pop("dns", None); p["route"].pop("default_domain_resolver", None)
p["route"]["rules"] = [r for r in p["route"]["rules"] if r.get("action") != "hijack-dns"]
print(json.dumps(p))
PY
c="hy2-e2e-noobfs-$sfx"
dk run -d --name "$c" --network "$net" --add-host "$host:$BR_IP" -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
  -v "$T/cl/sb-noobfs.json:/c.json:ro" "$sb_image" run -c /c.json >/dev/null
sleep 2
if via "$c" "http://$WEB_IP:8000/" >/dev/null; then nok "without the Salamander password the bridge must not answer"
else ok "without the Salamander password the bridge does not answer"; fi
docker rm -f "$c" >/dev/null 2>&1
# Revoking the device removes its credentials from both inbounds.
id1=$(echo "$d1" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
docker exec "$adm" curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' -d '{}' "http://$NC_TS/enroll/devices/$id1/revoke" >/dev/null \
  && ok "the admin revoked the sing-box device" || nok "revoke"
wait_for 60 docker exec "$br" sh -c "python3 -c \"import json;c=json.load(open('/etc/xray/config.json'));i={x['tag']:x for x in c['inbounds']};assert len(i['hy2-in']['settings']['clients'])==3\"" \
  && ok "its credentials left the Hysteria 2 inbound" || nok "credentials still on the bridge"
sleep 2
c=$(client sb direct-bridge-1-hy2)
sleep 2
if via "$c" "http://$WEB_IP:8000/" >/dev/null; then nok "a revoked device must not connect over Hysteria 2"
else ok "a revoked device cannot connect over Hysteria 2"; fi
docker rm -f "$c" >/dev/null 2>&1

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

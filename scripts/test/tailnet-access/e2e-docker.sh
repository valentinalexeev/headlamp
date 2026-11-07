#!/bin/sh
# On-demand end-to-end test of tailnet access through a bridge (#132). Run it with `make test-tailnet-access-e2e`.
# Needs docker. It reuses the bridge test image of the Hysteria 2 test (xray, the Go ingress agent, tailscaled).
#
# Two bridges join one tailnet: "bridge-off" (tailnet_access off, the default) and "bridge-all" (tailnet_access
# all). Two tailnet nodes run a web server that answers with the caller's address: "allowed" (tag:lan) and
# "denied" (tag:other). headscale keeps its policy in its database, and the node-control policy console changes it.
# The bridges' tag (tag:ingress) starts with no ACL rule at all.
#
# Matrix, on Hysteria 2 (sing-box) and VLESS Reality (mihomo):
#   before the ACL rule: bridge-all cannot reach "allowed" (the ACL decides, not the bridge)
#   after node-control writes the rule tag:ingress -> tag:lan:8000 and an admin confirms it:
#     bridge-all  -> allowed answers and sees the bridge; denied is refused; a private LAN address is refused
#     bridge-off  -> allowed, denied and the LAN address are all refused
#   the public web server works through both bridges.
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
#   E2E_SINGBOX_IMAGE    default ghcr.io/sagernet/sing-box:v1.14.4
#   E2E_MIHOMO_IMAGE     default metacubex/mihomo:v1.19.33
#   E2E_XRAY_VERSION     default 26.10.31
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
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
# 11.0.0.0/8 is not in geoip:private, so to the bridges this network is "the internet".
pub="11.$oct.8"; lan="172.31.$oct"
net="tna-e2e-$sfx"; lnet="tna-e2e-lan-$sfx"
hs="tna-e2e-hs-$sfx"; nc="tna-e2e-nc-$sfx"; adm="tna-e2e-admin-$sfx"; web="tna-e2e-web-$sfx"; lsv="tna-e2e-lansrv-$sfx"
fr="tna-e2e-front-$sfx"; tal="tna-e2e-allowed-$sfx"; tde="tna-e2e-denied-$sfx"
WEB_IP="$pub.80"; LAN_IP="$lan.80"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: networks $net $lnet, containers tna-e2e-*-$sfx, files $T"; return; fi
  docker ps -aq --filter "name=tna-e2e-.*-$sfx" | xargs -r docker rm -f >/dev/null 2>&1
  for n in "$net" "$lnet"; do dk network rm "$n" >/dev/null 2>&1; done; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() {
  echo "FATAL: $1"
  for c in "$hs" "$nc" "tna-e2e-bridge-off-$sfx" "tna-e2e-bridge-all-$sfx"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done
  exit 1
}
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# images"
dk build -q -t tna-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
mkdir -p "$T/bctx/agent"
cp -R "$root/apps/ingress-agent/go.mod" "$root/apps/ingress-agent/cmd" "$root/apps/ingress-agent/internal" "$T/bctx/agent/"
cp "$hyd/Dockerfile.bridge" "$hyd/bridge.sh" "$T/bctx/"
dk build -q --build-arg XRAY_VERSION="$xray_version" --build-arg TAILSCALE_IMAGE="$ts_image" -f "$T/bctx/Dockerfile.bridge" \
  -t tna-e2e-bridge "$T/bctx" >/dev/null || die "bridge image"
for i in "$hs_image" "$ts_image" "$sb_image" "$mh_image" python:3.13-alpine caddy:2-alpine curlimages/curl:8.17.0; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done

echo "# keys and certificates (all made here, for this run only)"
mkdir -p "$T/etc" "$T/tls" "$T/xray" "$T/nc" "$T/cl"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e test CA" \
  -keyout "$T/tls/ca.key" -out "$T/tls/ca.pem" >/dev/null 2>&1 || die "ca"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=bridges" -keyout "$T/tls/key.pem" -out "$T/tls/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=DNS:bridge-off.e2e.example,DNS:bridge-all.e2e.example\n' > "$T/tls/ext"
openssl x509 -req -in "$T/tls/csr.pem" -CA "$T/tls/ca.pem" -CAkey "$T/tls/ca.key" -CAcreateserial -days 1 \
  -extfile "$T/tls/ext" -out "$T/tls/cert.pem" >/dev/null 2>&1 || die "leaf"
chmod 644 "$T/tls/"*
kp=$(docker run --rm --entrypoint xray tna-e2e-bridge x25519) || die "x25519"
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
# No rule for tag:ingress: the bridges reach nothing on the tailnet until node-control adds one. The admin (an
# untagged node of e2e@) and node-control reach everything, so node-control can talk to the agents.
printf '%s' '{"tagOwners":{"tag:node-control":["e2e@"],"tag:ingress":["e2e@"],"tag:lan":["e2e@"],"tag:other":["e2e@"]},"acls":[{"action":"accept","src":["e2e@","tag:node-control"],"dst":["*:*"]}]}' \
  | hsx policy set -f /dev/stdin >/dev/null || die "bootstrap policy"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_bo=$(key tag:ingress); k_ba=$(key tag:ingress); k_al=$(key tag:lan); k_de=$(key tag:other)
k_ad=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_bo" ] && [ -n "$k_ba" ] && [ -n "$k_al" ] && [ -n "$k_de" ] && [ -n "$k_ad" ] && [ -n "$api" ] || die "keys"
joined() { [ -n "$(docker exec "$hs" headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o "\"given_name\": *\"$1\"")" ]; }
nodeip() { hsx nodes list -o json | python3 -c "import json,sys;print([n['ip_addresses'][0] for n in json.load(sys.stdin) if n.get('given_name')=='$1'][0])" 2>/dev/null; }
ok "headscale up, policy in the database"

echo "# tailnet targets: allowed (tag:lan) and denied (tag:other), each with a web server on 8000"
target() { # container name key
  dk run -d --name "$1" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$3" \
    -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME="$2" -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start $2"
  dk run -d --name "$1-web" --network "container:$1" python:3.13-alpine python3 -c "$echoer" >/dev/null || die "$2 web"
  wait_for 60 joined "$2" || die "$2 did not join"
}
target "$tal" allowed "$k_al"; target "$tde" denied "$k_de"
AL_TS=$(nodeip allowed); DE_TS=$(nodeip denied)

echo "# two bridges: xray with VLESS Reality and Hysteria 2, the agent, tailscaled"
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
    {"tag": "via-exit", "protocol": "socks", "settings": {"servers": [{"address": "127.0.0.1", "port": 1055}]}},
    {"tag": "via-tailnet", "protocol": "socks", "settings": {"servers": [{"address": "127.0.0.1", "port": 1055}]}}
  ],
  "routing": {"domainStrategy": "IPOnDemand", "rules": [
    {"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
    {"type": "field", "network": "tcp,udp", "outboundTag": "blocked"}
  ]}
}
JSON
chmod 644 "$T/xray/config.base.json"
bridge() { # name ip key
  dk run -d --name "tna-e2e-$1-$sfx" --network "$net" --ip "$2" \
    -v "$T/xray/config.base.json:/etc/xray/config.base.json:ro" -v "$T/tls:/etc/xray/tls:ro" \
    -e LOGIN_SERVER="http://$hs:8080" -e TS_AUTHKEY="$3" -e TS_HOSTNAME="$1" -e AGENT_TOKEN="$token" \
    -e INGRESS_NAME="$1" -e INGRESS_HOST="$1.e2e.example" -e INGRESS_PUBLIC_KEY="$pubk" -e INGRESS_SHORT_ID="$sid" \
    -e INGRESS_SNI="$front" -e INGRESS_HY2_PORT=443 tna-e2e-bridge >/dev/null || die "start $1"
  dk network connect --ip "$lan.$4" "$lnet" "tna-e2e-$1-$sfx" >/dev/null || die "$1 to the lan"
  wait_for 60 joined "$1" || die "$1 did not join"
}
BO_IP="$pub.21"; BA_IP="$pub.22"
bridge bridge-off "$BO_IP" "$k_bo" 21; bridge bridge-all "$BA_IP" "$k_ba" 22
BO_TS=$(nodeip bridge-off); BA_TS=$(nodeip bridge-all)
# The bridges reach the LAN themselves (control), so a refusal below is the agent's rule, not the network.
docker exec "tna-e2e-bridge-all-$sfx" curl -fsS --max-time 5 "http://$LAN_IP:8000/" >/dev/null 2>&1 \
  && ok "control: the bridge itself reaches the private LAN server" || die "the bridge does not reach the LAN (control)"

echo "# node-control: enrolment with bridge-all granting tailnet access, and the policy console"
cat > "$T/nc/enroll.json" <<JSON
{"profile_name": "tunnel", "link_host": "links.e2e.example", "ingresses": [
  {"name": "bridge-off", "host": "bridge-off.e2e.example", "public_key": "$pubk", "short_id": "$sid", "sni": "$front",
   "port": 443, "hy2_port": 443, "agent_url": "http://$BO_TS:9000", "token_env": "BRIDGE_TOKEN", "uuids": "base"},
  {"name": "bridge-all", "host": "bridge-all.e2e.example", "public_key": "$pubk", "short_id": "$sid", "sni": "$front",
   "port": 443, "hy2_port": 443, "agent_url": "http://$BA_TS:9000", "token_env": "BRIDGE_TOKEN", "uuids": "derived",
   "tailnet_access": "all"}]}
JSON
chmod 644 "$T/nc/enroll.json"
dk run -d --name "$nc" --network "$net" -v "$T/nc:/etc/nc:ro" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test -e POLICY_ADMIN_USERS=e2e@example.test \
  -e POLICY_DB=/tmp/policy.db -e ENROLL_ALLOWED_HOSTS=node-control \
  -e ENROLL_TAILNET_DST="tag:lan:8000" \
  -e ENROLL_CONFIG=/etc/nc/enroll.json -e ENROLL_DB=/tmp/enroll.db -e ENROLL_SECRET_KEY="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')" \
  -e BRIDGE_TOKEN="$token" -e HY2_OBFS_BRIDGE_OFF="$obfs" -e HY2_OBFS_BRIDGE_ALL="$obfs" tna-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 joined node-control || die "node-control did not join"
NC_TS=$(nodeip node-control)

echo "# an admin on the tailnet enrols a sing-box and a Clash device"
dk run -d --name "$adm" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$k_ad" \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=admin -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start admin"
dk exec "$adm" apk add --no-cache -q curl >/dev/null 2>&1 || die "admin tools"
# The admin calls node-control by name: the Host guard of the admin listener allows "node-control".
dk exec "$adm" sh -c "echo '$NC_TS node-control' >> /etc/hosts" || die "admin hosts"
A="http://node-control"
acurl() { docker exec "$adm" curl -sS --max-time 20 "$@"; }
wait_for 60 docker exec "$adm" curl -fsS --max-time 5 "$A/enroll/devices" || die "admin listener: $(docker logs --tail 5 "$nc" 2>&1)"
exp=$(( $(date +%s) + 86400 ))
newdev() { acurl -f -X POST -H 'Content-Type: application/json' \
  -d "{\"name\":\"$1\",\"owner\":\"e2e\",\"client\":\"$2\",\"expires_at\":$exp}" "$A/enroll/devices"; }
d1=$(newdev phone sing-box) || die "create device: $(docker logs --tail 5 "$nc" 2>&1)"
d2=$(newdev laptop clash) || die "create clash device"
s1=$(echo "$d1" | sed -n 's#.*"subscription_url":"https://[^/]*/sub/\([^"]*\)".*#\1#p')
s2=$(echo "$d2" | sed -n 's#.*"subscription_url":"https://[^/]*/sub/\([^"]*\)".*#\1#p')
[ -n "$s1" ] && [ -n "$s2" ] && ok "the admin enrolled a sing-box and a Clash device" || die "no subscription urls ($d1)"
acurl -f -A e2e-client "http://$NC_TS:8081/sub/$s1" > "$T/cl/singbox.json" || die "sing-box subscription"
acurl -f -A e2e-client "http://$NC_TS:8081/sub/$s2" > "$T/cl/clash.json" || die "clash subscription"

grants() { # bridge -> the number of credentials with a tailnet grant in the bridge's rendered config
  docker exec "tna-e2e-$1-$sfx" python3 -c "
import json; c = json.load(open('/etc/xray/config.json'))
r = [x for x in c['routing']['rules'] if x.get('outboundTag') == 'via-tailnet']
print(len(r[0]['user']) if r else 0)" 2>/dev/null
}
wait_for 60 sh -c "[ \"\$(docker exec tna-e2e-bridge-all-$sfx python3 -c \"import json;c=json.load(open('/etc/xray/config.json'));print(len(c['inbounds'][0]['settings']['clients']))\")\" = 6 ]" \
  || die "credentials not applied on bridge-all"
[ "$(grants bridge-all)" = 6 ] && ok "bridge-all: all six credentials have the tailnet grant" || nok "bridge-all grants: $(grants bridge-all)"
[ "$(grants bridge-off)" = 0 ] && ok "bridge-off: no credential has a tailnet grant" || nok "bridge-off grants: $(grants bridge-off)"

echo "# clients"
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
pin_mh() { python3 - "$T/cl/clash.json" "$1" "$BO_IP" "$BA_IP" > "$T/cl/mh-$1.yaml" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p.update({"mixed-port": 2080, "bind-address": "127.0.0.1", "allow-lan": False, "rules": ["MATCH," + sys.argv[2]]})
p["hosts"] = {"bridge-off.e2e.example": sys.argv[3], "bridge-all.e2e.example": sys.argv[4]}
p["dns"] = {"enable": True, "use-hosts": True, "nameserver": ["system"], "proxy-server-nameserver": ["system"]}
print(json.dumps(p))
PY
}
client() { # kind tag -> starts the client, prints the container name
  c="tna-e2e-$1-$(echo "$2" | tr -c 'a-z0-9-\n' '-')-$sfx"
  docker rm -f "$c" >/dev/null 2>&1
  hosts="--add-host bridge-off.e2e.example:$BO_IP --add-host bridge-all.e2e.example:$BA_IP"
  if [ "$1" = sb ]; then pin_sb "$2"
    # shellcheck disable=SC2086
    dk run -d --name "$c" --network "$net" $hosts -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
      -v "$T/cl/sb-$2.json:/c.json:ro" "$sb_image" run -c /c.json >/dev/null || return 1
  else pin_mh "$2"
    mkdir -p "$T/cl/mh-$2.d"; cp "$T/cl/mh-$2.yaml" "$T/cl/mh-$2.d/config.yaml"
    # shellcheck disable=SC2086
    dk run -d --name "$c" --network "$net" $hosts -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
      -v "$T/cl/mh-$2.d:/conf" "$mh_image" -d /conf >/dev/null || return 1
  fi
  echo "$c"
}
via() { docker run --rm --network "container:$1" curlimages/curl:8.17.0 -fsS --max-time 8 --socks5-hostname 127.0.0.1:2080 "$2" 2>/dev/null; }
up() { # container -> waits until the client reaches the public web server
  i=0; while [ $i -lt 15 ]; do via "$1" "http://$WEB_IP:8000/" >/dev/null && return 0; i=$((i + 1)); sleep 1; done; return 1
}
refused() { # container url label
  if via "$1" "$2" >/dev/null; then nok "$3 must be refused"; else ok "$3 is refused"; fi
}
answers() { # container url want label
  got=""; i=0; while [ $i -lt 10 ]; do got=$(via "$1" "$2") && break; i=$((i + 1)); sleep 1; done
  if [ "$got" = "$3" ]; then ok "$4 answers and sees $3"; else nok "$4: want $3, got '$got'"; fi
}
AL="http://$AL_TS:8000/"; DE="http://$DE_TS:8000/"; LANURL="http://$LAN_IP:8000/"

echo "# before the ACL rule"
for kind in sb mh; do
  if [ $kind = sb ]; then sfx2=-hy2; lbl="Hysteria 2 (sing-box)"; else sfx2=; lbl="VLESS Reality (mihomo)"; fi
  c=$(client $kind "direct-bridge-all$sfx2") || { nok "$lbl: client start"; continue; }
  up "$c" && ok "$lbl via bridge-all: the public web server answers" || nok "$lbl via bridge-all: no public path"
  refused "$c" "$AL" "$lbl via bridge-all, before the ACL rule: the allowed host"
  docker rm -f "$c" >/dev/null 2>&1
done

echo "# node-control writes the ACL rule, and the admin confirms it"
st=$(acurl "$A/enroll/tailnet-acl")
echo "$st" | grep -q '"status":"out_of_sync"' && ok "the console reports the rule as missing" || nok "tailnet-acl state: $st"
res=$(acurl -X POST -H 'Content-Type: application/json' -d '{}' "$A/enroll/tailnet-acl")
cid=$(echo "$res" | sed -n 's/.*"change_id":\([0-9]*\).*/\1/p')
[ -n "$cid" ] && ok "node-control submitted the rule as policy change $cid" || die "apply: $res"
code=$(acurl -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"change_id\":$cid}" "$A/policy/confirm")
[ "$code" = 204 ] && ok "the admin confirmed the change" || die "confirm: $code"
hsx policy get 2>/dev/null | python3 -c "
import json,sys; p=json.load(sys.stdin)
assert {'action':'accept','src':['tag:ingress'],'dst':['tag:lan:8000']} in p['acls'], p['acls']" \
  && ok "the live policy has tag:ingress -> tag:lan:8000" || nok "the live policy lacks the rule"
acurl "$A/enroll/tailnet-acl" | grep -q '"status":"in_sync"' && ok "the console reports the rule in sync" || nok "not in sync"
sleep 3

echo "# after the ACL rule"
for kind in sb mh; do
  if [ $kind = sb ]; then sfx2=-hy2; lbl="Hysteria 2 (sing-box)"; else sfx2=; lbl="VLESS Reality (mihomo)"; fi
  c=$(client $kind "direct-bridge-all$sfx2") || { nok "$lbl: client start"; continue; }
  up "$c" || nok "$lbl via bridge-all: no public path"
  answers "$c" "$AL" "$BA_TS" "$lbl via bridge-all: the allowed host"
  refused "$c" "$DE" "$lbl via bridge-all: the denied host"
  refused "$c" "$LANURL" "$lbl via bridge-all: a private LAN address"
  docker rm -f "$c" >/dev/null 2>&1
  c=$(client $kind "direct-bridge-off$sfx2") || { nok "$lbl: client start"; continue; }
  up "$c" && ok "$lbl via bridge-off: the public web server answers" || nok "$lbl via bridge-off: no public path"
  refused "$c" "$AL" "$lbl via bridge-off: the allowed host"
  refused "$c" "$DE" "$lbl via bridge-off: the denied host"
  refused "$c" "$LANURL" "$lbl via bridge-off: a private LAN address"
  docker rm -f "$c" >/dev/null 2>&1
done

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

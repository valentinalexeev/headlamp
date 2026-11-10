#!/bin/sh
# On-demand end-to-end test of OIDC self-enrolment (#102). Run it with `make test-oidc-e2e`. Needs docker.
#
# A real identity provider (dex, static users, a groups claim), real headscale, node-control, one bridge (the Hysteria 2
# test image: xray, the Go agent, tailscaled) and Caddy as the link host front with TLS. A browser stand-in (curl with
# a cookie jar) signs in to dex, enrols a device on /me, and gets a relay subscription and a tailnet key.
#   - the relay works: a sing-box client through the bridge reaches a web server
#   - the tailnet key works: a tailscale client joins as the person's own headscale user
#   - a person outside the allowed group cannot sign in
#   - an admin locks the person: the relay client is refused, the tailnet node is expired, and sign-in is refused
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
#   E2E_SINGBOX_IMAGE    default ghcr.io/sagernet/sing-box:v1.14.4
#   E2E_DEX_IMAGE        default ghcr.io/dexidp/dex:v2.44.0
#   E2E_XRAY_VERSION     default 26.10.31
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hyd="$root/scripts/test/hysteria"
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
sb_image=${E2E_SINGBOX_IMAGE:-ghcr.io/sagernet/sing-box:v1.14.4}
dex_image=${E2E_DEX_IMAGE:-ghcr.io/dexidp/dex:v2.44.0}
xray_version=${E2E_XRAY_VERSION:-26.10.31}
front=front.e2e.example; links=links.e2e.example; idp=idp.e2e.example; bridge_host=bridge.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v openssl >/dev/null 2>&1 || skip "openssl is not available"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 180 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
pub="11.$oct.9"
net="oidc-e2e-$sfx"
hs="oidc-e2e-hs-$sfx"; nc="oidc-e2e-nc-$sfx"; br="oidc-e2e-bridge-$sfx"; dx="oidc-e2e-dex-$sfx"; edge="oidc-e2e-edge-$sfx"
web="oidc-e2e-web-$sfx"; fr="oidc-e2e-front-$sfx"; adm="oidc-e2e-admin-$sfx"; ua="oidc-e2e-ua-$sfx"; tsc="oidc-e2e-tsc-$sfx"
WEB_IP="$pub.80"; BR_IP="$pub.20"; EDGE_IP="$pub.30"; DEX_IP="$pub.40"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers oidc-e2e-*-$sfx, files $T"; return; fi
  docker ps -aq --filter "name=oidc-e2e-.*-$sfx" | xargs -r docker rm -f >/dev/null 2>&1
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() {
  echo "FATAL: $1"
  for c in "$dx" "$nc" "$edge" "$br"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done
  exit 1
}
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# images"
dk build -q -t oidc-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
mkdir -p "$T/bctx/agent"
cp -R "$root/apps/ingress-agent/go.mod" "$root/apps/ingress-agent/cmd" "$root/apps/ingress-agent/internal" "$T/bctx/agent/"
cp "$hyd/Dockerfile.bridge" "$hyd/bridge.sh" "$T/bctx/"
dk build -q --build-arg XRAY_VERSION="$xray_version" --build-arg TAILSCALE_IMAGE="$ts_image" -f "$T/bctx/Dockerfile.bridge" \
  -t oidc-e2e-bridge "$T/bctx" >/dev/null || die "bridge image"
for i in "$hs_image" "$ts_image" "$sb_image" "$dex_image" python:3.13-alpine caddy:2-alpine curlimages/curl:8.17.0; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done

echo "# keys and certificates (all made here, for this run only)"
mkdir -p "$T/etc" "$T/tls" "$T/xray" "$T/nc" "$T/cl" "$T/dex" "$T/edge"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e test CA" \
  -keyout "$T/tls/ca.key" -out "$T/tls/ca.pem" >/dev/null 2>&1 || die "ca"
leaf() { # name -> $T/tls/<name>.pem and .key
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1" -keyout "$T/tls/$1.key" -out "$T/tls/$1.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:%s\n' "$1" > "$T/tls/$1.ext"
  openssl x509 -req -in "$T/tls/$1.csr" -CA "$T/tls/ca.pem" -CAkey "$T/tls/ca.key" -CAcreateserial -days 1 \
    -extfile "$T/tls/$1.ext" -out "$T/tls/$1.pem" >/dev/null 2>&1 || die "leaf $1"
}
leaf "$bridge_host"; leaf "$links"; leaf "$idp"
cp "$T/tls/$bridge_host.pem" "$T/tls/cert.pem"; cp "$T/tls/$bridge_host.key" "$T/tls/key.pem"
chmod 644 "$T/tls/"*
kp=$(docker run --rm --entrypoint xray oidc-e2e-bridge x25519) || die "x25519"
priv=$(echo "$kp" | sed -n 's/^Private[ _]*[Kk]ey: *//p')
pubk=$(echo "$kp" | sed -n -e 's/^Public[ _]*[Kk]ey: *//p' -e 's/^Password[^:]*: *//p' | head -1)
[ -n "$priv" ] && [ -n "$pubk" ] || die "reality keys"
sid=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
obfs=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
csecret=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')

echo "# network, web server and Reality front"
dk network create --subnet "$pub.0/24" "$net" >/dev/null || die "network"
echoer='import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s):
        b = s.client_address[0].encode(); s.send_response(200); s.send_header("Content-Length", str(len(b))); s.end_headers(); s.wfile.write(b)
    def log_message(s, *a): pass
h.ThreadingHTTPServer(("0.0.0.0", 8000), E).serve_forever()'
dk run -d --name "$web" --network "$net" --ip "$WEB_IP" python:3.13-alpine python3 -c "$echoer" >/dev/null || die web
printf '%s {\n\ttls internal\n\trespond "front"\n}\n' "$front" > "$T/Caddyfile.front"
dk run -d --name "$fr" --network "$net" -v "$T/Caddyfile.front:/etc/caddy/Caddyfile:ro" caddy:2-alpine >/dev/null || die front

echo "# identity provider: dex with two static people; only anna is allowed"
cat > "$T/dex/config.yaml" <<YAML
issuer: https://$idp
storage: {type: memory}
web:
  https: 0.0.0.0:443
  tlsCert: /tls/$idp.pem
  tlsKey: /tls/$idp.key
oauth2:
  skipApprovalScreen: true
  passwordConnector: local
enablePasswordDB: true
staticClients:
  - id: node-control
    secret: $csecret
    name: node-control
    redirectURIs: ["https://$links/oidc/callback"]
staticPasswords:
  - email: anna@example.test
    hash: "\$2a\$14\$Fo8bA7DvsCE0PRhB8rjs0.8T5fKlXcX4Wds2KZHeRjWTv3ixTOnYe"
    username: anna
    userID: "a1111111-0000-4000-8000-000000000001"
  - email: bob@example.test
    hash: "\$2a\$14\$Fo8bA7DvsCE0PRhB8rjs0.8T5fKlXcX4Wds2KZHeRjWTv3ixTOnYe"
    username: bob
    userID: "b2222222-0000-4000-8000-000000000002"
YAML
chmod 644 "$T/dex/config.yaml"
# dex's password database has no groups. The allow-list therefore uses the email claim: OIDC_GROUPS_CLAIM=email and
# OIDC_ALLOWED_GROUPS=anna@example.test. The code path is the same as for a real groups claim.
dk run -d --name "$dx" --network "$net" --ip "$DEX_IP" --network-alias "$idp" -v "$T/dex:/etc/dex:ro" -v "$T/tls:/tls:ro" \
  "$dex_image" dex serve /etc/dex/config.yaml >/dev/null || die "start dex"

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
  magic_dns: false
  override_local_dns: false
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /data/headscale.sock
log:
  level: info
YAML
cat > "$T/etc/acl.json" <<'JSON'
{"tagOwners": {"tag:node-control": ["e2e@"], "tag:ingress": ["e2e@"]}, "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]}
JSON
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_br=$(key tag:ingress); k_ad=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_br" ] && [ -n "$k_ad" ] && [ -n "$api" ] || die "keys"
joined() { [ -n "$(docker exec "$hs" headscale -c /etc/headscale/config.yaml nodes list -o json | grep -o "\"given_name\": *\"$1\"")" ]; }
nodeip() { hsx nodes list -o json | python3 -c "import json,sys;print([n['ip_addresses'][0] for n in json.load(sys.stdin) if n.get('given_name')=='$1'][0])" 2>/dev/null; }
ok "headscale and the identity provider are up"

echo "# bridge"
cat > "$T/xray/config.base.json" <<JSON
{
  "log": {"loglevel": "warning", "access": "none"},
  "stats": {}, "api": {"tag": "api", "services": ["StatsService"]},
  "policy": {"levels": {"0": {"statsUserUplink": true, "statsUserDownlink": true}}},
  "inbounds": [
    {"tag": "reality-in", "listen": "0.0.0.0", "port": 443, "protocol": "vless",
     "settings": {"clients": [], "decryption": "none"},
     "streamSettings": {"network": "tcp", "security": "reality",
       "realitySettings": {"dest": "$fr:443", "serverNames": ["$front"], "privateKey": "$priv", "shortIds": ["$sid"]}}},
    {"tag": "hy2-in", "listen": "0.0.0.0", "port": 443, "protocol": "hysteria",
     "settings": {"version": 2, "clients": []},
     "streamSettings": {"network": "hysteria", "hysteriaSettings": {"version": 2},
       "security": "tls", "tlsSettings": {"alpn": ["h3"],
         "certificates": [{"certificateFile": "/etc/xray/tls/cert.pem", "keyFile": "/etc/xray/tls/key.pem"}]},
       "finalmask": {"udp": [{"type": "salamander", "settings": {"password": "$obfs"}}]}}},
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
  -e INGRESS_NAME=bridge-1 -e INGRESS_HOST="$bridge_host" -e INGRESS_PUBLIC_KEY="$pubk" -e INGRESS_SHORT_ID="$sid" \
  -e INGRESS_SNI="$front" -e INGRESS_HY2_PORT=443 oidc-e2e-bridge >/dev/null || die "start bridge"
wait_for 60 joined bridge || die "bridge did not join"
BR_TS=$(nodeip bridge)

echo "# node-control with OIDC self-enrolment, behind a TLS front on the link host"
cat > "$T/nc/enroll.json" <<JSON
{"profile_name": "tunnel", "link_host": "$links", "ingresses": [
  {"name": "bridge-1", "host": "$bridge_host", "public_key": "$pubk", "short_id": "$sid", "sni": "$front", "port": 443,
   "hy2_port": 443, "agent_url": "http://$BR_TS:9000", "token_env": "BRIDGE_TOKEN", "uuids": "base"}]}
JSON
chmod 644 "$T/nc/enroll.json"
dk run -d --name "$nc" --network "$net" --network-alias node-control -v "$T/nc:/etc/nc:ro" -v "$T/tls/ca.pem:/etc/ssl/certs/e2e-ca.pem:ro" \
  -e SSL_CERT_FILE=/etc/ssl/certs/e2e-ca.pem \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test -e ENROLL_ALLOWED_HOSTS=node-control \
  -e ENROLL_CONFIG=/etc/nc/enroll.json -e ENROLL_DB=/tmp/enroll.db -e ENROLL_SECRET_KEY="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')" \
  -e BRIDGE_TOKEN="$token" -e HY2_OBFS_BRIDGE_1="$obfs" \
  -e OIDC_ISSUER="https://$idp" -e OIDC_CLIENT_ID=node-control -e OIDC_CLIENT_SECRET="$csecret" \
  -e OIDC_REDIRECT_URL="https://$links/oidc/callback" -e OIDC_DB=/tmp/oidc.db \
  -e OIDC_GROUPS_CLAIM=email -e OIDC_ALLOWED_GROUPS=anna@example.test -e OIDC_SCOPES="openid email profile offline_access" \
  --add-host "$idp:$DEX_IP" oidc-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 joined node-control || die "node-control did not join: $(docker logs --tail 5 "$nc" 2>&1)"
NC_TS=$(nodeip node-control)
# The link host front: TLS for the link host, proxied to node-control's public listener, which is on the tailnet only.
# Caddy shares the network of a tailscale node. In production this is the bridge's Caddy.
dk run -d --name "$edge-ts" --network "$net" --ip "$EDGE_IP" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$(key "")" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=edge -e TS_USERSPACE=false \
  "$ts_image" >/dev/null || die "start edge tailscale"
cat > "$T/edge/Caddyfile" <<CADDY
{
	auto_https disable_redirects
}
https://$links {
	tls /tls/$links.pem /tls/$links.key
	reverse_proxy $NC_TS:8081
}
CADDY
dk run -d --name "$edge" --network "container:$edge-ts" -v "$T/edge/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$T/tls:/tls:ro" \
  caddy:2-alpine >/dev/null || die "start edge"
wait_for 60 joined edge || die "edge did not join"

echo "# a browser stand-in signs in"
dk run -d --name "$ua" --network "$net" --add-host "$links:$EDGE_IP" --add-host "$idp:$DEX_IP" -v "$T/tls/ca.pem:/ca.pem:ro" \
  --entrypoint sleep curlimages/curl:8.17.0 3600 >/dev/null || die "start browser"
wait_for 60 docker exec "$ua" curl -fsS --cacert /ca.pem "https://$links/me" || die "the /me page is not reachable: $(docker logs --tail 5 "$nc" 2>&1)"
# signin <user> <jar>: follows /oidc/login to dex's login form, posts the password, and follows back to /me.
signin() {
  docker exec "$ua" sh -c "
    set -e
    rm -f /tmp/$2
    c='curl -sS --cacert /ca.pem -b /tmp/$2 -c /tmp/$2'
    # login -> dex auth -> (connector) -> local login form URL
    form=\$(\$c -L -o /dev/null -w '%{url_effective}' https://$links/oidc/login)
    # post the password; dex redirects to the approval-less callback, which sets the session and redirects to /me
    \$c -L -o /tmp/$2.page -w '%{http_code} %{url_effective}' --data-urlencode login=$1@example.test --data-urlencode password=e2e-password \"\$form\"
  "
}
out=$(signin anna jar-anna) || die "anna sign-in: $out"
docker exec "$ua" grep -q "Signed in as anna@example.test" /tmp/jar-anna.page \
  && ok "anna signed in through the identity provider" || die "anna is not signed in: $out $(docker exec "$ua" cat /tmp/jar-anna.page | head -c 400)"
out=$(signin bob jar-bob)
docker exec "$ua" grep -q "not in a group" /tmp/jar-bob.page \
  && ok "bob, outside the allowed group, cannot sign in" || nok "bob: $out"

echo "# anna enrols a device"
csrf=$(docker exec "$ua" sed -n 's/.*name="csrf" value="\([0-9a-f]*\)".*/\1/p' /tmp/jar-anna.page | head -1)
[ -n "$csrf" ] || die "no csrf"
docker exec "$ua" curl -sS --cacert /ca.pem -b /tmp/jar-anna -c /tmp/jar-anna -o /tmp/enrol.page \
  --data-urlencode "csrf=$csrf" --data-urlencode name=Phone --data-urlencode client=sing-box "https://$links/me/devices" || die "enrol"
# The page lists one URL per profile format (#241); the relay check below runs sing-box, so take that one by name.
sub=$(docker exec "$ua" sed -n 's#.*<code>https://[^/]*/sub/\([^<]*/sing-box\)</code>.*#\1#p' /tmp/enrol.page | head -1)
tkey=$(docker exec "$ua" sed -n 's#.*<code>\(hskey-[^<]*\)</code>.*#\1#p' /tmp/enrol.page | head -1)
[ -z "$tkey" ] && tkey=$(docker exec "$ua" sh -c "grep -o '<code>[^<]*</code>' /tmp/enrol.page | sed 's#</*code>##g' | grep -v '^https://' | head -1")
[ -n "$sub" ] && [ -n "$tkey" ] && ok "anna got a relay subscription and a tailnet key, once" || die "enrol page: $(docker exec "$ua" head -c 600 /tmp/enrol.page)"
hsx users list -o json | grep -q '"name": *"oidc-' && ok "headscale has a user of her own" || nok "no oidc- user in headscale"

echo "# the relay works"
# The enrolment pushes at once; wait until xray runs with the three new credentials on both inbounds.
wait_for 60 docker exec "$br" sh -c "python3 -c \"import json;c=json.load(open('/etc/xray/config.json'));i={x['tag']:x for x in c['inbounds']};assert len(i['hy2-in']['settings']['clients'])==3\"" \
  || die "the bridge did not apply the device"
sleep 2
docker exec "$ua" curl -fsS --cacert /ca.pem -A e2e-client "https://$links/sub/$sub" > "$T/cl/singbox.json" || die "subscription"
python3 - "$T/cl/singbox.json" > "$T/cl/sb.json" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["inbounds"] = [{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": 2080}]
p["route"]["final"] = "direct-bridge-1-hy2"
p.pop("dns", None); p["route"].pop("default_domain_resolver", None)
p["route"]["rules"] = [r for r in p["route"]["rules"] if r.get("action") != "hijack-dns"]
print(json.dumps(p))
PY
sbc="oidc-e2e-sb-$sfx"
dk run -d --name "$sbc" --network "$net" --add-host "$bridge_host:$BR_IP" -e SSL_CERT_FILE=/ca.pem -v "$T/tls/ca.pem:/ca.pem:ro" \
  -v "$T/cl/sb.json:/c.json:ro" "$sb_image" run -c /c.json >/dev/null || die "sing-box"
via() { docker run --rm --network "container:$sbc" curlimages/curl:8.17.0 -fsS --max-time 6 --socks5-hostname 127.0.0.1:2080 "http://$WEB_IP:8000/" 2>/dev/null; }
if wait_for 45 via; then ok "anna's relay device reaches the web through the bridge"
else nok "relay does not work: $(docker logs --tail 5 "$sbc" 2>&1 | tr '\n' ' ')"; fi

echo "# the tailnet key works"
dk run -d --name "$tsc" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$tkey" \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=anna-laptop -e TS_USERSPACE=false "$ts_image" >/dev/null || die "tailscale client"
wait_for 60 joined anna-laptop && ok "anna's laptop joined the tailnet with the key" || nok "laptop did not join"
owner=$(hsx nodes list -o json | python3 -c "import json,sys;print([n['user']['name'] for n in json.load(sys.stdin) if n.get('given_name')=='anna-laptop'][0])" 2>/dev/null)
case "$owner" in oidc-*) ok "the laptop belongs to her own headscale user ($owner)";; *) nok "laptop owner: $owner";; esac

echo "# an admin locks anna"
dk run -d --name "$adm" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -e TS_AUTHKEY="$k_ad" \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" -e TS_HOSTNAME=admin -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start admin"
dk exec "$adm" apk add --no-cache -q curl >/dev/null 2>&1 || die "admin tools"
dk exec "$adm" sh -c "echo '$NC_TS node-control' >> /etc/hosts"
wait_for 60 docker exec "$adm" curl -fsS --max-time 5 http://node-control/enroll/people || die "admin listener"
person=$(docker exec "$adm" curl -fsS http://node-control/enroll/people | python3 -c "import json,sys;print([p['subject'] for p in json.load(sys.stdin)['people'] if p['email']=='anna@example.test'][0])")
[ -n "$person" ] && ok "the console lists anna" || die "people list"
enc=$(python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1], safe=''))" "$person")
res=$(docker exec "$adm" curl -sS -X POST -H 'Content-Type: application/json' -d '{"reason":"e2e"}' "http://node-control/enroll/people/$enc/lock")
echo "$res" | grep -q '"ok":true' && ok "the lock completed" || nok "lock: $res"
sleep 3
if via; then nok "after the lock the relay device must be refused"; else ok "after the lock the relay device is refused"; fi
hsx nodes list -o json | python3 -c "
import json,sys,datetime
n=[n for n in json.load(sys.stdin) if n.get('given_name')=='anna-laptop'][0]
e=n.get('expiry')
# the CLI prints a protobuf timestamp ({\"seconds\": N}); other versions print RFC 3339 text
sec=int(e.get('seconds', 0)) if isinstance(e, dict) else int(datetime.datetime.fromisoformat(str(e).replace('Z','+00:00')).timestamp()) if e and not str(e).startswith('0001') else 0
assert 0 < sec <= datetime.datetime.now(datetime.timezone.utc).timestamp() + 1, e" \
  && ok "her tailnet node is expired" || nok "the tailnet node is not expired"
out=$(signin anna jar-anna2)
docker exec "$ua" grep -q "locked" /tmp/jar-anna2.page && ok "anna cannot sign in again" || nok "sign-in after lock: $out"
docker exec "$ua" curl -sS --cacert /ca.pem -b /tmp/jar-anna "https://$links/me" | grep -q "Signed in as" \
  && nok "her old session must end with the lock" || ok "her old session ended"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

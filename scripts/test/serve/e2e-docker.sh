#!/bin/sh
# On-demand end-to-end test of serve (#117) against a real headscale. Not part of the unit tests; run it with
# `make test-serve-e2e`. Needs docker. It builds the node-control and funnel-sidecar images from this tree.
#
# On one docker network: headscale (MagicDNS on, the serve zone sent to node-control with dns.nameservers.split),
# node-control with the serve module, two plain HTTP backends, two funnel-sidecars in serve mode (one per service),
# and a tailscale client. An admin creates an exact service and a wildcard service through the API; each sidecar
# joins with its service's key and names nothing. From the client the test checks that the names resolve to the
# sidecars and that HTTPS verifies against the serve CA and reaches the right backend.
#   E2E_HEADSCALE_IMAGE  default jauderho/headscale:v0.29.3
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.98.10
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-jauderho/headscale:v0.29.3}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.98.10}
base=tailnet.e2e.test
zone=svc.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="serve-e2e-$sfx"
hs="serve-e2e-hs-$sfx"; nc="serve-e2e-nc-$sfx"; cl="serve-e2e-cl-$sfx"
be1="serve-e2e-be1-$sfx"; be2="serve-e2e-be2-$sfx"; sc1="serve-e2e-sc1-$sfx"; sc2="serve-e2e-sc2-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers serve-e2e-*-$sfx"; return; fi
  for c in "$cl" "$sc1" "$sc2" "$be1" "$be2" "$nc" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$hs" "$nc" "$sc1" "$sc2" "$cl"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# building images"
dk build -q -t serve-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
dk build -q -f "$root/apps/funnel-ingress/Dockerfile.sidecar" -t serve-e2e-sidecar "$root/apps/funnel-ingress" >/dev/null || die "sidecar image"
for i in "$hs_image" "$ts_image" python:3.13-alpine; do dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"; done

mkdir -p "$T/etc"
# node-control joins first, so it gets the first tailnet address, which the split names.
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
  base_domain: $base
  override_local_dns: false
  nameservers:
    global: []
    split:
      $zone:
        - 100.64.0.1
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /data/headscale.sock
log:
  level: info
YAML
cat > "$T/etc/acl.json" <<'JSON'
{
  "tagOwners": {"tag:node-control": ["e2e@"], "tag:serve": ["e2e@"], "tag:ns-mon": ["e2e@"], "tag:ns-k8s": ["e2e@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON

dk network create "$net" >/dev/null || die "network"

echo "# headscale"
dk run -d --name "$hs" --network "$net" --entrypoint "" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  sh -c 'mkdir -p /data && exec headscale serve -c /etc/headscale/config.yaml' >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_cl=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_cl" ] && [ -n "$api" ] || die "keys"
ok "headscale up"

echo "# node-control (serve on; it mints the service keys itself)"
dk run -d --name "$nc" --network "$net" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test \
  -e SERVE_DB=/tmp/serve.db -e SERVE_ZONE="$zone" -e SERVE_BASE_DOMAIN="$base" -e SERVE_USER=e2e -e SERVE_DNS_REFRESH=3s \
  -e NODE_CONTROL_STATE_KEY="$(head -c 32 /dev/urandom | base64)" \
  serve-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list | grep -q node-control" || die "node-control did not join"
# the headscale CLI prints snake_case JSON; the node-control client also accepts it
NC=$(docker exec "$hs" sh -c "headscale -c /etc/headscale/config.yaml nodes list -o json" | python3 -c 'import json,sys;print([n["ip_addresses"][0] for n in json.load(sys.stdin) if n.get("given_name")=="node-control"][0])' 2>/dev/null)
[ "$NC" = 100.64.0.1 ] && ok "node-control is 100.64.0.1, the address the split names" || nok "node-control is 100.64.0.1 (got $NC)"

echo "# client"
dk run -d --name "$cl" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$k_cl" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --accept-dns=true" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start client"
dk exec "$cl" apk add --no-cache -q curl bind-tools jq >/dev/null 2>&1 || die "client tools"
wait_for 60 docker exec "$cl" curl -fsS --max-time 5 "http://100.64.0.1/serve/ca.crt" -o /tmp/ca.crt || die "admin listener"
ok "the client gets the serve CA"

api_post() { docker exec "$cl" curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' -d "$2" "http://100.64.0.1$1"; }
grafana=$(api_post /serve/services '{"name":"grafana","namespace":"mon"}') || die "create grafana: $(docker logs --tail 5 "$nc" 2>&1)"
apps=$(api_post /serve/services '{"name":"*.apps","namespace":"k8s"}') || die "create *.apps"
gk=$(echo "$grafana" | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p'); ak=$(echo "$apps" | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p')
[ -n "$gk" ] && [ -n "$ak" ] && ok "the admin created grafana.$zone and *.apps.$zone; each has its own key" || die "no keys returned"
# the key is reusable, ephemeral and tagged by node-control, in headscale
keys=$(docker exec "$hs" headscale -c /etc/headscale/config.yaml preauthkeys list -o json)
if echo "$keys" | python3 -c 'import json,sys;sys.exit(0 if any(k.get("reusable") and k.get("ephemeral") and set(k.get("acl_tags") or [])=={"tag:serve","tag:ns-mon"} for k in json.load(sys.stdin)) else 1)'; then
  ok "headscale holds a reusable ephemeral key with tag:serve and tag:ns-mon"
else nok "headscale holds a reusable ephemeral key with tag:serve and tag:ns-mon ($(echo "$keys" | python3 -c 'import json,sys;print([(k["id"],k.get("reusable"),k.get("ephemeral"),k.get("acl_tags")) for k in json.load(sys.stdin)])'))"; fi

echo "# backends and sidecars (the sidecars name nothing)"
backend() { dk run -d --name "$1" --network "$net" python:3.13-alpine sh -c "mkdir -p /w && echo $2 > /w/index.html && exec python3 -m http.server 3000 -d /w" >/dev/null || die "backend $1"; }
backend "$be1" grafana-ok; backend "$be2" ingress-ok
sidecar() { dk run -d --name "$1" --network "$net" -e FUNNEL_CONTROL_URL="http://$hs:8080" -e TS_AUTHKEY="$2" \
  -e FUNNEL_HOSTNAME="$3" -e FUNNEL_SERVICES="web=443@$4:3000" -e FUNNEL_SERVE_URL=http://100.64.0.1 serve-e2e-sidecar >/dev/null || die "sidecar $1"; }
sidecar "$sc1" "$gk" grafana "$be1"; sidecar "$sc2" "$ak" ingress "$be2"
wait_for 60 sh -c "docker logs $sc1 2>&1 | grep -q 'certificate for grafana.$zone'" && ok "the grafana sidecar got a certificate for grafana.$zone" || nok "grafana sidecar certificate ($(docker logs --tail 3 "$sc1" 2>&1 | tr '\n' ' '))"
wait_for 60 sh -c "docker logs $sc2 2>&1 | grep -q 'certificate for \*.apps.$zone'" && ok "the ingress sidecar got a certificate for *.apps.$zone" || nok "ingress sidecar certificate ($(docker logs --tail 3 "$sc2" 2>&1 | tr '\n' ' '))"
S1=$(docker exec "$cl" tailscale ip -4 grafana 2>/dev/null); S2=$(docker exec "$cl" tailscale ip -4 ingress 2>/dev/null)

echo "# DNS and HTTPS from the client"
resolves() { [ "$(docker exec "$cl" dig +short +time=2 +tries=1 @100.100.100.100 "$1" A | tail -1)" = "$2" ]; }
for pair in "grafana.$zone $S1" "a.apps.$zone $S2" "x.y.apps.$zone $S2"; do
  set -- $pair
  wait_for 40 resolves "$1" "$2" && ok "$1 resolves to $2 through the split" || nok "$1 resolves to $2 (got: $(docker exec "$cl" dig +short @100.100.100.100 "$1" A | tr '\n' ' '))"
done
rc=$(docker exec "$cl" dig +time=2 +tries=1 @100.100.100.100 "nope.$zone" A | grep -o 'status: [A-Z]*')
[ "$rc" = "status: NXDOMAIN" ] && ok "an unknown name in the zone is NXDOMAIN" || nok "unknown name ($rc)"
wait_for 20 sh -c "docker exec $cl getent ahostsv4 grafana.$zone | grep -q $S1" && ok "the OS resolver finds grafana.$zone ($S1)" || nok "the OS resolver finds grafana.$zone ($(docker exec "$cl" getent ahostsv4 "grafana.$zone" | head -1))"

https() { docker exec "$cl" curl -fsS --max-time 10 --cacert /tmp/ca.crt "https://$1/"; }
body=""; i=0; while [ $i -lt 20 ]; do body=$(https "grafana.$zone" 2>/dev/null) && break; i=$((i + 1)); sleep 2; done
[ "$body" = grafana-ok ] && ok "HTTPS to grafana.$zone verifies and reaches its backend" || nok "HTTPS grafana ($(https "grafana.$zone" 2>&1 | tail -1))"
body=""; i=0; while [ $i -lt 20 ]; do body=$(https "a.apps.$zone" 2>/dev/null) && break; i=$((i + 1)); sleep 2; done
[ "$body" = ingress-ok ] && ok "HTTPS to a.apps.$zone verifies (wildcard) and reaches the ingress backend" || nok "HTTPS a.apps ($(https "a.apps.$zone" 2>&1 | tail -1))"
if https "x.y.apps.$zone" >/dev/null 2>&1; then nok "x.y.apps must not verify (a wildcard covers one label)"
else ok "x.y.apps.$zone does not verify: a wildcard certificate covers one label"; fi
if docker exec "$cl" curl -fsS --max-time 10 "https://grafana.$zone/" >/dev/null 2>&1; then nok "without the serve CA the name must not verify"
else ok "without the serve CA the name does not verify"; fi

echo "# replicas and deletion"
sidecar "serve-e2e-sc3-$sfx" "$gk" grafana "$be1"
wait_for 60 sh -c "docker exec $cl dig +short @100.100.100.100 grafana.$zone A | grep -c . | grep -qx 2" && ok "a second replica on the grafana key: the name answers both" || nok "two replicas ($(docker exec "$cl" dig +short @100.100.100.100 "grafana.$zone" A | tr '\n' ' '))"
docker rm -f "serve-e2e-sc3-$sfx" >/dev/null 2>&1
gid=$(echo "$grafana" | sed -n 's/.*"service":{"id":"\([^"]*\)".*/\1/p')
docker exec "$cl" curl -fsS --max-time 20 -X DELETE "http://100.64.0.1/serve/services/$gid" >/dev/null && ok "the admin deleted grafana" || nok "delete grafana"
wait_for 20 sh -c "docker exec $cl dig +time=2 +tries=1 @100.100.100.100 grafana.$zone A | grep -q NXDOMAIN" && ok "grafana.$zone is gone from DNS" || nok "grafana gone from DNS"
# its key is expired: a new node cannot join with it
lost="serve-e2e-lost-$sfx"
dk run -d --name "$lost" --network "$net" -e TS_AUTHKEY="$gk" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" \
  -e TS_USERSPACE=true -e TS_HOSTNAME=late-replica "$ts_image" >/dev/null 2>&1
sleep 15
if hsx nodes list 2>/dev/null | grep -q late-replica; then nok "a replica with the deleted service key must not join"
else ok "a replica with the deleted service key cannot join"; fi
docker rm -f "$lost" >/dev/null 2>&1

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

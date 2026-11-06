#!/bin/sh
# On-demand end-to-end test of serve (#117) against a real headscale. Not part of the unit tests; run it with
# `make test-serve-e2e`. Needs docker. It builds the node-control and funnel-sidecar images from this tree.
#
# On one docker network it starts: headscale (MagicDNS on, dns.extra_records_path set), node-control with the serve
# module, `node-control serve-sync` on the headscale volume, a plain HTTP backend, funnel-sidecar in serve mode, and
# a tailscale client. It then checks, from the client, that the name resolves through MagicDNS to the sidecar and that
# HTTPS to the name verifies against the serve CA and reaches the backend.
#   E2E_HEADSCALE_IMAGE  default jauderho/headscale:v0.29.3
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.98.10
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-jauderho/headscale:v0.29.3}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.98.10}
base=e2e.example.test
name=web.cluster.$base

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="serve-e2e-$sfx"; vol="serve-e2e-$sfx"
hs="serve-e2e-hs-$sfx"; nc="serve-e2e-nc-$sfx"; sync="serve-e2e-sync-$sfx"
be="serve-e2e-be-$sfx"; sc="serve-e2e-sc-$sfx"; cl="serve-e2e-cl-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers serve-e2e-*-$sfx"; return; fi
  for c in "$cl" "$sc" "$be" "$sync" "$nc" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk volume rm -f "$vol" >/dev/null 2>&1; dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$hs" "$nc" "$sync" "$sc" "$cl"; do echo "--- logs $c"; docker logs --tail 30 "$c" 2>&1; done; exit 1; }
# wait_for DESCRIPTION SECONDS COMMAND...: retry until the command succeeds
wait_for() { d=$1; n=$2; shift 2; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# building images"
dk build -q -t serve-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
dk build -q -f "$root/apps/funnel-ingress/Dockerfile.sidecar" -t serve-e2e-sidecar "$root/apps/funnel-ingress" >/dev/null || die "sidecar image"
for i in "$hs_image" "$ts_image" alpine:3.22 python:3.13-alpine; do dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"; done

mkdir -p "$T/etc"
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
  extra_records_path: /data/extra-records.json
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /data/headscale.sock
log:
  level: info
YAML
cat > "$T/etc/acl.json" <<'JSON'
{
  "tagOwners": {
    "tag:node-control": ["e2e@"], "tag:headscale": ["e2e@"], "tag:serve": ["e2e@"],
    "tag:ns-demo": ["e2e@"]
  },
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON
# headscale reads the file at start; serve-sync replaces it later
printf '[]\n' > "$T/extra-records.json"

dk network create "$net" >/dev/null || die "network"
dk volume create "$vol" >/dev/null || die "volume"
# seed the volume with the empty records file, owned for the sync agent (it runs as root in this image)
dk run --rm -v "$vol:/data" -v "$T:/seed:ro" alpine:3.22 cp /seed/extra-records.json /data/extra-records.json || die "seed"

echo "# headscale"
dk run -d --name "$hs" --network "$net" --entrypoint "" -v "$vol:/data" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  headscale serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for headscale 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" --reusable -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
# the client is a user's device (no tag): the admin console refuses tagged nodes
k_nc=$(key tag:node-control); k_sync=$(key tag:headscale); k_sc=$(key tag:serve,tag:ns-demo); k_cl=$(key "")
api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_sync" ] && [ -n "$k_sc" ] && [ -n "$k_cl" ] && [ -n "$api" ] || die "keys"
ok "headscale up, keys created"

echo "# node-control"
dk run -d --name "$nc" --network "$net" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e SERVE_DB=/tmp/serve.db -e SERVE_BASE_DOMAIN="$base" -e ADMIN_USERS=e2e@example.test \
  -e NODE_CONTROL_STATE_KEY="$(head -c 32 /dev/urandom | base64)" \
  serve-e2e-node-control >/dev/null || die "start node-control"

echo "# client"
dk run -d --name "$cl" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$k_cl" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --accept-dns=true" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false -e TS_STATE_DIR=/var/lib/tailscale "$ts_image" >/dev/null || die "start client"
dk exec "$cl" apk add --no-cache -q curl bind-tools >/dev/null 2>&1 || die "client tools"
ncip() { dk exec "$cl" tailscale ip -4 node-control 2>/dev/null; }
wait_for "node-control on tailnet" 60 sh -c "docker exec $cl tailscale ip -4 node-control | grep -q ." || die "node-control did not join"
NC=$(ncip)
wait_for "node-control admin" 30 docker exec "$cl" curl -fsS "http://$NC/serve/ca.crt" -o /tmp/ca.crt || die "admin listener"
ok "node-control joined ($NC) and serves the CA"

# the namespace demo may name hosts under cluster.<base>. Serve admin routes need a role even with the console open,
# so the client's user is an owner (ADMIN_USERS).
code=$(dk exec "$cl" curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' \
  -d '{"suffixes":["cluster"]}' "http://$NC/serve/namespaces/demo")
case "$code" in 2*) ok "namespace demo created";; *) nok "namespace demo created (HTTP $code; $(docker logs "$nc" 2>&1 | grep authz | tail -1))";; esac

echo "# serve-sync next to headscale"
dk run -d --name "$sync" --network "$net" -v "$vol:/data" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e SERVE_SYNC_AUTH_KEY="$k_sync" -e SERVE_SYNC_URL="http://$NC" \
  -e SERVE_RECORDS_PATH=/data/extra-records.json -e SERVE_BASE_DOMAIN="$base" -e SERVE_SYNC_SECONDS=5 \
  serve-e2e-node-control serve-sync >/dev/null || die "start serve-sync"

echo "# backend and sidecar"
dk run -d --name "$be" --network "$net" python:3.13-alpine sh -c 'mkdir -p /w && echo serve-e2e-ok > /w/index.html && exec python3 -m http.server 3000 -d /w' \
  >/dev/null || die "start backend"
dk run -d --name "$sc" --network "$net" \
  -e FUNNEL_CONTROL_URL="http://$hs:8080" -e TS_AUTHKEY="$k_sc" -e FUNNEL_HOSTNAME=web-pod \
  -e FUNNEL_NS=demo -e FUNNEL_SERVICES="web=443@$be:3000" \
  -e FUNNEL_SERVE_URL="http://$NC" -e FUNNEL_SERVE_NAMES="$name" \
  serve-e2e-sidecar >/dev/null || die "start sidecar"
wait_for "sidecar on tailnet" 60 sh -c "docker exec $cl tailscale ip -4 web-pod | grep -q ." || die "sidecar did not join"
SC=$(dk exec "$cl" tailscale ip -4 web-pod)
ok "sidecar joined ($SC)"

# serve-sync polls every 5 s; headscale watches the file and pushes the record to clients
if wait_for "record in headscale file" 60 sh -c "docker exec $hs grep -q '$name' /data/extra-records.json"; then
  ok "serve-sync wrote $name to dns.extra_records_path"
else nok "serve-sync wrote $name to dns.extra_records_path"; fi

if wait_for "MagicDNS answer" 60 sh -c "docker exec $cl dig +short @100.100.100.100 $name A | grep -qx '$SC'"; then
  ok "$name resolves to $SC through MagicDNS"
else nok "$name resolves to $SC through MagicDNS (got: $(docker exec "$cl" dig +short @100.100.100.100 "$name" A))"; fi

body=""
i=0; while [ $i -lt 30 ]; do
  body=$(docker exec "$cl" curl -fsS --max-time 10 --cacert /tmp/ca.crt "https://$name/" 2>/dev/null) && break
  i=$((i + 1)); sleep 2
done
[ "$body" = serve-e2e-ok ] && ok "HTTPS to $name verifies against the serve CA and reaches the backend" \
  || nok "HTTPS to $name (last error: $(docker exec "$cl" curl -sS --max-time 10 --cacert /tmp/ca.crt "https://$name/" 2>&1 | tail -1))"

# negative: the certificate does not verify for a name the sidecar did not claim
if docker exec "$cl" curl -fsS --max-time 10 --cacert /tmp/ca.crt --resolve "other.cluster.$base:443:$SC" "https://other.cluster.$base/" >/dev/null 2>&1; then
  nok "a name not claimed does not verify"
else ok "a name not claimed does not verify"; fi
# negative: the CA is the trust anchor, not the system store
if docker exec "$cl" curl -fsS --max-time 10 "https://$name/" >/dev/null 2>&1; then nok "without the serve CA the name does not verify"
else ok "without the serve CA the name does not verify"; fi

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

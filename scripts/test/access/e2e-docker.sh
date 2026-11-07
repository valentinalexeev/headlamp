#!/bin/sh
# On-demand end-to-end test of the Access page (#123) against a real headscale. Run it with `make test-access-e2e`.
# Needs docker. It builds the node-control image from this tree.
#
# headscale, node-control (ADMIN_USERS=owner, ADMIN_DB set) and two tailscale clients: one as the owner, one as a
# second user. The owner grants viewer to the second user on the Access page API. The second user can then read the
# nodes but gets 403 on start, and loses access when the grant is removed, with no restart. The test also checks that
# the second user cannot grant itself, and that the file owner survives an empty import.
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
#   E2E_KEEP=1           keep the containers on exit, for debugging
# shellcheck disable=SC2015 # ok() always succeeds, so A && ok || nok is safe
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="access-e2e-$sfx"
hs="access-e2e-hs-$sfx"; nc="access-e2e-nc-$sfx"; own="access-e2e-own-$sfx"; usr="access-e2e-usr-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers access-e2e-*-$sfx"; return; fi
  for c in "$usr" "$own" "$nc" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$hs" "$nc" "$own" "$usr"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# building images"
dk build -q -t access-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
for i in "$hs_image" "$ts_image"; do dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"; done

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
{
  "tagOwners": {"tag:node-control": ["owner@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON

dk network create "$net" >/dev/null || die "network"

echo "# headscale"
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" "$hs_image" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create owner --email owner@example.test >/dev/null || die "user owner"
hsx users create bob --email bob@example.test >/dev/null || die "user bob"
uid() { hsx users list -o json | python3 -c "import json,sys;print([u['id'] for u in json.load(sys.stdin) if u['name']=='$1'][0])"; }
u_own=$(uid owner); u_bob=$(uid bob)
key() { hsx preauthkeys create -u "$1" -e 1h ${2:+--tags "$2"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key "$u_own" tag:node-control); k_own=$(key "$u_own"); k_bob=$(key "$u_bob"); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_own" ] && [ -n "$k_bob" ] && [ -n "$api" ] || die "keys"
ok "headscale up with two users"

echo "# node-control (file owner owner@, Access page on ADMIN_DB)"
dk run -d --name "$nc" --network "$net" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=owner@ -e ADMIN_DB=/tmp/admin.db \
  access-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list | grep -q node-control" || die "node-control did not join"
NC=$(docker exec "$hs" headscale -c /etc/headscale/config.yaml nodes list -o json | python3 -c 'import json,sys;print([n["ip_addresses"][0] for n in json.load(sys.stdin) if n.get("given_name")=="node-control"][0])' 2>/dev/null)
[ -n "$NC" ] || die "node-control address"

client() {
  dk run -d --name "$1" --network "$net" -e TS_AUTHKEY="$2" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" \
    -e TS_HOSTNAME="$3" -e TS_USERSPACE=false --cap-add NET_ADMIN --device /dev/net/tun "$ts_image" >/dev/null || die "start $1"
  dk exec "$1" apk add --no-cache -q curl >/dev/null 2>&1 || die "tools on $1"
}
client "$own" "$k_own" owner-laptop; client "$usr" "$k_bob" bob-laptop
code() { c=$1; m=$2; p=$3; shift 3; docker exec "$c" curl -s -o /dev/null -w '%{http_code}' --max-time 15 -X "$m" "$@" "http://$NC$p"; }
post() { code "$1" "$2" "$3" -H 'Content-Type: application/json' -d "$4"; }
wait_for 60 sh -c "[ \"\$(docker exec $own curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$NC/admin/state)\" = 200 ]" || die "admin listener"
ok "the file owner opens the Access page"

[ "$(code "$usr" GET /nodes.json)" = 403 ] && ok "bob has no grant: GET /nodes.json is 403" || nok "bob without grant"
[ "$(code "$usr" GET /admin/state)" = 403 ] && ok "bob cannot open the Access page" || nok "bob Access page"
[ "$(post "$usr" POST /admin/grants '{"subject":"bob@","roles":["owner"]}')" = 403 ] && ok "bob cannot grant itself" || nok "bob self-grant"

[ "$(post "$own" POST /admin/grants '{"subject":"bob@","roles":["viewer"]}')" = 200 ] && ok "the owner grants viewer to bob" || nok "grant viewer"
[ "$(code "$usr" GET /nodes.json)" = 200 ] && ok "bob views nodes" || nok "bob views nodes"
[ "$(post "$usr" POST /nodes/x/start '{}')" = 403 ] && ok "bob gets 403 on start" || nok "bob start"
[ "$(post "$own" POST /admin/grants '{"subject":"owner@","roles":["viewer"]}')" = 403 ] && ok "the owner cannot change its own grants" || nok "owner self-grant"

[ "$(post "$own" POST /admin/grants '{"subject":"bob@","roles":[]}')" = 200 ] && ok "the owner removes the grant" || nok "revoke"
[ "$(code "$usr" GET /nodes.json)" = 403 ] && ok "bob is refused again, no restart" || nok "revoke takes effect"

[ "$(post "$own" POST /admin/import '{"grants":[{"subject":"bob@","roles":["operator"]}]}')" = 200 ] && ok "import" || nok "import"
docker exec "$own" curl -fsS --max-time 15 "http://$NC/admin/export" | grep -q '"operator"' && ok "export returns the imported grant" || nok "export"
[ "$(post "$own" POST /admin/import '{"grants":[]}')" = 200 ] && [ "$(code "$own" GET /admin/state)" = 200 ] \
  && ok "after an empty import the file owner keeps access (break-glass)" || nok "break-glass"
docker exec "$own" curl -fsS --max-time 15 "http://$NC/admin/audit" | grep -q '"import"' && ok "the change log records the import" || nok "audit"

echo "# restart keeps the stored grants"
post "$own" POST /admin/grants '{"subject":"bob@","roles":["viewer"]}' >/dev/null
dk restart "$nc" >/dev/null || die "restart"
wait_for 60 sh -c "[ \"\$(docker exec $usr curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$NC/nodes.json)\" = 200 ]" \
  && ok "after a restart, the stored grant still applies" || nok "grant after restart"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

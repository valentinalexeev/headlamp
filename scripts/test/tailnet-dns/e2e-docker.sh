#!/bin/sh
# On-demand end-to-end test of the tailnet DNS module (#286) against a real headscale. Not part of the unit tests; run
# it with `make test-tailnet-dns-e2e`. Needs docker. It builds the node-control image from this tree.
#
# On one docker network: headscale with dns.extra_records_path in a directory it shares with node-control, node-control
# with the tailnet DNS module and TAILNET_DNS_EXTRA_RECORDS_FILE, two nodes with tag:llm-proxy, and a tailscale client.
# An admin adds records through the API. The test checks that node-control writes them to the file, that headscale
# sends them to the client in the netmap (the client resolves the name itself), that node-control answers all
# addresses of a name (the bridge path), that the file follows a node that goes offline, and that a delete removes the
# name from the client.
#   E2E_HEADSCALE_IMAGE     default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE     default tailscale/tailscale:v1.104.4 (the client; needs curl, or apk to install it)
#   E2E_NODE_CONTROL_IMAGE  a prebuilt node-control image; default: build one from apps/node-control
#   E2E_KEEP=1              keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
nc_image=${E2E_NODE_CONTROL_IMAGE:-}
base=tailnet.e2e.test
name=proxy.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="tdns-e2e-$sfx"
hs="tdns-e2e-hs-$sfx"; nc="tdns-e2e-nc-$sfx"; cl="tdns-e2e-cl-$sfx"; t1="tdns-e2e-t1-$sfx"; t2="tdns-e2e-t2-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers tdns-e2e-*-$sfx, files $T"; return; fi
  for c in "$cl" "$t1" "$t2" "$nc" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$hs" "$nc" "$cl"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# images"
if [ -z "$nc_image" ]; then
  nc_image=tdns-e2e-node-control
  dk build -q -t "$nc_image" "$root/apps/node-control" >/dev/null || die "node-control image"
fi
for i in "$hs_image" "$ts_image"; do dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"; done

# The shared directory: headscale reads the file, node-control replaces it by rename. headscale does not start while
# the file is missing, so it starts with an empty list.
mkdir -p "$T/etc" "$T/dns"
printf '[]\n' > "$T/dns/extra-records.json"
chmod 755 "$T/dns"; chmod 644 "$T/dns/extra-records.json"
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
  extra_records_path: /dns/extra-records.json
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /data/headscale.sock
log:
  level: info
YAML
cat > "$T/etc/acl.json" <<'JSON'
{
  "tagOwners": {"tag:node-control": ["e2e@"], "tag:llm-proxy": ["e2e@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON

dk network create "$net" >/dev/null || die "network"

echo "# headscale"
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" -v "$T/dns:/dns:ro" "$hs_image" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
ok "headscale starts with an empty extra records file"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_cl=$(key ""); k_t=$(key tag:llm-proxy); k_t2=$(key tag:llm-proxy)
api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_cl" ] && [ -n "$k_t" ] && [ -n "$k_t2" ] && [ -n "$api" ] || die "keys"

echo "# node-control (tailnet DNS on; it owns the extra records file)"
dk run -d --name "$nc" --network "$net" -v "$T/dns:/dns" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test \
  -e TAILNET_DNS_DB=/tmp/dns.db -e TAILNET_DNS_REFRESH=2s -e TAILNET_DNS_EXTRA_RECORDS_FILE=/dns/extra-records.json \
  "$nc_image" >/dev/null || die "start node-control"
wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list | grep -q node-control" || die "node-control did not join"

echo "# two tagged nodes and a client"
target() { dk run -d --name "$1" --network "$net" -e TS_AUTHKEY="$2" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" \
  -e TS_HOSTNAME="$3" -e TS_USERSPACE=true "$ts_image" >/dev/null || die "start $1"; }
target "$t1" "$k_t" llm-a; target "$t2" "$k_t2" llm-b
dk run -d --name "$cl" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$k_cl" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --accept-dns=true" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start client"
command -v curl >/dev/null 2>&1; docker exec "$cl" sh -c 'command -v curl' >/dev/null 2>&1 \
  || dk exec "$cl" apk add --no-cache -q curl >/dev/null 2>&1 || die "client tools"
wait_for 60 sh -c "docker exec $cl tailscale ip -4 llm-a && docker exec $cl tailscale ip -4 llm-b" || die "targets did not join"
A1=$(docker exec "$cl" tailscale ip -4 llm-a); A2=$(docker exec "$cl" tailscale ip -4 llm-b)
NC=$(docker exec "$cl" tailscale ip -4 node-control)
wait_for 60 docker exec "$cl" curl -fsS --max-time 5 "http://$NC/dns/state" -o /tmp/state.json || die "admin listener"
ok "the client reaches node-control at $NC"

api() { docker exec "$cl" curl -fsS --max-time 20 -X "$1" -H 'Content-Type: application/json' ${3:+-d "$3"} "http://$NC$2"; }
# the name resolved by the client itself (tailscale answers from the netmap), and by node-control (the bridge path)
own() { docker exec "$cl" nslookup -type=a "$1" 100.100.100.100 2>/dev/null | awk '/^Name:/{n=1;next} n&&/^Address/{print $NF}'; }
ncq() { docker exec "$cl" nslookup -type=a "$1" "$NC" 2>/dev/null | awk '/^Name:/{n=1;next} n&&/^Address/{print $NF}' | sort | tr '\n' ' ' | sed 's/ $//'; }
# the IPv4 values of the name in the file (it also holds the AAAA records)
file_values() { python3 -c 'import json,sys;print(" ".join(sorted(e["value"] for e in json.load(open(sys.argv[1])) if e["type"]=="A" and e["name"]==sys.argv[2])))' "$T/dns/extra-records.json" "$name"; }
both=$(printf '%s\n%s\n' "$A1" "$A2" | sort | tr '\n' ' ' | sed 's/ $//')

echo "# a record for the tag: node-control writes it, headscale sends it"
api POST /dns/records "{\"name\":\"$name\",\"tag\":\"tag:llm-proxy\"}" >/dev/null && ok "the admin added $name -> tag:llm-proxy" || die "create record"
file_is() { i=0; while [ $i -lt "$1" ]; do [ "$(file_values)" = "$2" ] && return 0; i=$((i + 1)); sleep 1; done; return 1; }
file_is 20 "$both" && ok "the file has one A record per online node ($both)" || nok "file values ($(file_values))"
grep -q '"AAAA"' "$T/dns/extra-records.json" && ok "the file also has the AAAA records" || nok "no AAAA records in the file"
grep -q '"type": "A"' "$T/dns/extra-records.json" && ok "the file uses the headscale format (name, type, value)" || nok "file format: $(cat "$T/dns/extra-records.json")"
wait_for 30 sh -c "docker exec $cl tailscale debug netmap | grep -c '\"Value\": \"100\\.' | grep -qx 2" \
  && ok "headscale sends both records in the netmap" || nok "netmap records ($(docker exec "$cl" tailscale debug netmap | grep -A2 ExtraRecords | tr -d ' \n'))"
got=$(own "$name")
case " $both " in *" $got "*) ok "the client resolves $name itself ($got, one address: tailscale answers the first)";;
  *) nok "the client resolves $name (got: $got)";; esac
[ "$(ncq "$name")" = "$both" ] && ok "node-control answers all addresses of $name (the bridge path)" || nok "node-control answer ($(ncq "$name"))"

echo "# a node goes offline: the file follows"
docker rm -f "$t2" >/dev/null 2>&1
file_is 90 "$A1" \
  && ok "the file drops the offline node ($A1 stays)" || nok "after llm-b went offline: $(file_values)"
wait_for 30 sh -c "[ \"\$(docker exec $cl nslookup -type=a $name 100.100.100.100 2>/dev/null | awk '/^Name:/{n=1;next} n&&/^Address/{print \$NF}')\" = \"$A1\" ]" \
  && ok "the client resolves $name to $A1" || nok "client after failover ($(own "$name"))"

echo "# fixed addresses and the state"
api POST /dns/records '{"name":"fixed.e2e.example","addresses":["100.64.0.99"]}' >/dev/null || nok "create fixed"
wait_for 30 sh -c "[ \"\$(docker exec $cl nslookup -type=a fixed.e2e.example 100.100.100.100 2>/dev/null | awk '/^Name:/{n=1;next} n&&/^Address/{print \$NF}')\" = 100.64.0.99 ]" \
  && ok "a fixed record reaches the client" || nok "fixed record ($(own fixed.e2e.example))"
state=$(api GET /dns/state)
echo "$state" | grep -q '"extra_records_file":"/dns/extra-records.json"' && ok "/dns/state names the file" || nok "state file ($state)"
echo "$state" | grep -q '"split":{}' && ok "/dns/state gives no split lines when the file is on" || nok "state split ($state)"

echo "# delete"
api DELETE "/dns/records/$name" >/dev/null && ok "the admin deleted $name" || nok "delete"
wait_for 30 sh -c "! grep -q '$name' $T/dns/extra-records.json" && ok "$name leaves the file" || nok "file after delete: $(cat "$T/dns/extra-records.json")"
wait_for 30 sh -c "[ -z \"\$(docker exec $cl nslookup -type=a $name 100.100.100.100 2>/dev/null | awk '/^Name:/{n=1;next} n&&/^Address/{print \$NF}')\" ]" \
  && ok "the client no longer resolves $name" || nok "client after delete ($(own "$name"))"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

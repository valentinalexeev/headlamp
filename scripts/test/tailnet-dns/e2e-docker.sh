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
# #285: two more clients join with tag:dev-a and tag:dev-b. The test adds a zone, a record with rules per tag, and
# CNAME, TXT and SRV records, then checks that each client gets the answer of its own rule from node-control (WhoIs on
# the query source), that a record with rules stays out of the file, and that an unknown name in the zone is NXDOMAIN.
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
ca="tdns-e2e-ca-$sfx"; cb="tdns-e2e-cb-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers tdns-e2e-*-$sfx, files $T"; return; fi
  for c in "$cl" "$ca" "$cb" "$t1" "$t2" "$nc" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
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
  "tagOwners": {"tag:node-control": ["e2e@"], "tag:llm-proxy": ["e2e@"], "tag:dev-a": ["e2e@"], "tag:dev-b": ["e2e@"]},
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
k_a=$(key tag:dev-a); k_b=$(key tag:dev-b)
api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_cl" ] && [ -n "$k_t" ] && [ -n "$k_t2" ] && [ -n "$k_a" ] && [ -n "$k_b" ] && [ -n "$api" ] || die "keys"

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

echo "# #285: two clients with different tags get different answers"
# A client in kernel mode, so its queries to node-control leave from its own tailnet address.
tun_client() { dk run -d --name "$1" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$2" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --accept-dns=false" -e TS_HOSTNAME="$3" \
  -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start $1"; }
tun_client "$ca" "$k_a" dev-a; tun_client "$cb" "$k_b" dev-b
wait_for 60 sh -c "docker exec $ca tailscale ip -4 && docker exec $cb tailscale ip -4" || die "tagged clients did not join"
wait_for 60 sh -c "docker exec $ca tailscale ping -c 1 $NC && docker exec $cb tailscale ping -c 1 $NC" || nok "tagged clients reach node-control"
# q <container> <type> <name>: the answers of node-control, sorted, on one line
q() { docker exec "$1" nslookup -type="$2" "$3" "$NC" 2>&1 | awk -v t="$2" '
  /^Name:/{n=1;next} n&&/^Address/{print $NF;next}
  t=="txt"&&/text =/{sub(/.*text = /,"");print;next}
  t=="srv"&&/service =/{print $(NF);next}
  t=="cname"&&/canonical name =/{print $NF;next}
  /NXDOMAIN/{print "NXDOMAIN"}' | sort | tr '\n' ' ' | sed 's/ $//'; }
api POST /dns/zones '{"name":"corp.e2e.example"}' >/dev/null && ok "the admin added the zone corp.e2e.example" || nok "create zone"
api POST /dns/records "{\"name\":\"svc.corp.e2e.example\",\"addresses\":[\"100.64.0.99\"],\"rules\":[{\"tags\":[\"tag:dev-a\"],\"addresses\":[\"100.64.0.11\"]},{\"tags\":[\"tag:dev-b\"],\"node\":\"llm-a\"}]}" >/dev/null \
  && ok "the admin added svc.corp.e2e.example with a rule per tag" || nok "create record with rules"
# q_is <tries> <container> <name> <want>: wait until node-control answers want (the first path to it can take a moment)
q_is() { i=0; while [ $i -lt "$1" ]; do [ "$(q "$2" a "$3")" = "$4" ] && return 0; i=$((i + 1)); sleep 1; done; return 1; }
q_is 30 "$ca" svc.corp.e2e.example 100.64.0.11; q_is 30 "$cb" svc.corp.e2e.example "$A1"
got_a=$(q "$ca" a svc.corp.e2e.example); got_b=$(q "$cb" a svc.corp.e2e.example); got_c=$(q "$cl" a svc.corp.e2e.example)
[ "$got_a" = 100.64.0.11 ] && ok "the tag:dev-a client gets its rule ($got_a)" || nok "tag:dev-a answer ($got_a)"
[ "$got_b" = "$A1" ] && ok "the tag:dev-b client gets the node of its rule ($got_b)" || nok "tag:dev-b answer ($got_b, want $A1)"
[ "$got_c" = 100.64.0.99 ] && ok "a client that no rule matches gets the default ($got_c)" || nok "default answer ($got_c)"
[ "$got_a" != "$got_b" ] && ok "the two tagged clients get different answers" || nok "same answer for both tags"
[ "$(q "$ca" a nothing.corp.e2e.example)" = NXDOMAIN ] && ok "an unknown name in the zone is NXDOMAIN" || nok "unknown name in zone ($(q "$ca" a nothing.corp.e2e.example))"
! grep -q svc.corp.e2e.example "$T/dns/extra-records.json" && ok "a record with rules stays out of the extra records file" || nok "rules record in the file"
api POST /dns/records '{"name":"alias.corp.e2e.example","type":"CNAME","target":"svc.corp.e2e.example"}' >/dev/null || nok "create CNAME"
api POST /dns/records '{"name":"info.corp.e2e.example","type":"TXT","text":["v=e2e"]}' >/dev/null || nok "create TXT"
api POST /dns/records '{"name":"_http._tcp.corp.e2e.example","type":"SRV","target":"svc.corp.e2e.example","port":8080}' >/dev/null || nok "create SRV"
case "$(q "$cb" a alias.corp.e2e.example)" in *"$A1"*) ok "a CNAME follows to the rule of the client";; *) nok "CNAME ($(q "$cb" a alias.corp.e2e.example))";; esac
case "$(q "$ca" txt info.corp.e2e.example)" in *v=e2e*) ok "a TXT record answers";; *) nok "TXT ($(q "$ca" txt info.corp.e2e.example))";; esac
case "$(q "$ca" srv _http._tcp.corp.e2e.example)" in *svc.corp.e2e.example*) ok "an SRV record answers";; *) nok "SRV ($(q "$ca" srv _http._tcp.corp.e2e.example))";; esac
state=$(api GET /dns/state)
echo "$state" | grep -q '"corp.e2e.example":\["' && ok "/dns/state gives the split line of the zone" || nok "zone split ($state)"
docker exec "$cl" curl -fsS --max-time 10 "http://$NC/dns" | grep -q '<title>DNS</title>' && ok "the console serves the DNS page" || nok "DNS page"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

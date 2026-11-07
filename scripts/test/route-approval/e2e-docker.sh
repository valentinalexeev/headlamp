#!/bin/sh
# e2e (#152): route approval through the policy alone (autoApprovers), on a given headscale image, with real tailscale
# clients. No API key and no approver script. Usage: scripts/test/route-approval/e2e-docker.sh <headscale image> [upgrade image]
# With an upgrade image: the state is built on the first image, then headscale is replaced in place by the second.
set -u
img=${1:-ghcr.io/juanfont/headscale:$(tr -d '[:space:]' < "$(dirname "$0")/../../../HEADSCALE_VERSION")}
up=${2:-}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
sfx=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
net="hlra-$sfx"; lnet="hlra-lan-$sfx"; pub="11.$oct.3"; lan="172.29.$oct"
hs="hlra-hs-$sfx"; T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: hlra-*-$sfx"; return; fi
  docker ps -aq --filter "name=hlra-.*-$sfx" | xargs -r docker rm -f >/dev/null 2>&1
  docker volume ls -q --filter "name=hlra-.*-$sfx" | xargs -r docker volume rm >/dev/null 2>&1
  docker network rm "$net" "$lnet" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; docker logs --tail 20 "$hs" 2>&1; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

docker network create --subnet "$pub.0/24" "$net" >/dev/null || die net
docker network create --internal --subnet "$lan.0/24" "$lnet" >/dev/null || die lan
mkdir -p "$T/etc" "$T/ca"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=spike CA" \
  -keyout "$T/ca/ca.key" -out "$T/ca/ca.pem" >/dev/null 2>&1 || die ca
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$hs" -keyout "$T/etc/key.pem" -out "$T/ca/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s\n' "$hs" > "$T/ca/ext"
openssl x509 -req -in "$T/ca/csr.pem" -CA "$T/ca/ca.pem" -CAkey "$T/ca/ca.key" -CAcreateserial -days 1 -extfile "$T/ca/ext" \
  -out "$T/etc/cert.pem" >/dev/null 2>&1 || die leaf
chmod 644 "$T/etc/"*.pem "$T/ca/ca.pem"
cat > "$T/etc/config.yaml" <<YAML
server_url: https://$hs
listen_addr: 0.0.0.0:443
tls_cert_path: /etc/headscale/cert.pem
tls_key_path: /etc/headscale/key.pem
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
    region_code: ra
    region_name: ra
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /data/derp.key
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
unix_socket: /data/hs.sock
log:
  level: info
YAML
chmod 644 "$T/etc/config.yaml"
start_hs() { # image
  docker run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" -v "hlra-data-$sfx:/data" "$1" \
    serve -c /etc/headscale/config.yaml >/dev/null || die "start $1"
  wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale $1 not ready"
}
hx() { docker exec -i "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
start_hs "$img"
echo "# headscale: $(hx version 2>/dev/null | head -1)"
hx users create _svcs >/dev/null; hx users create alice >/dev/null
# The site's policy shape: IPv4 split in two halves for tag:gateway, exit nodes for tag:exit, nothing for users. The
# example acl.json files also list the IPv6 halves; they are left out here so that an IPv6 route is a route outside it.
cat <<'JSON' | hx policy set -f /dev/stdin >/dev/null || die "policy"
{
  "tagOwners": {"tag:gateway": ["_svcs@"], "tag:exit": ["_svcs@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
  "autoApprovers": {
    "exitNode": ["tag:exit"],
    "routes": {"0.0.0.0/1": ["tag:gateway"], "128.0.0.0/1": ["tag:gateway"]}
  }
}
JSON
uid() { hx users list -o json | python3 -c "import json,sys;print([u['id'] for u in json.load(sys.stdin) if u['name']=='$1'][0])"; }
key() { hx preauthkeys create -u "$(uid "$1")" -e 1h --reusable ${2:+--tags "$2"} -o json | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])"; }
k_gw=$(key _svcs tag:gateway); k_ex=$(key _svcs tag:exit); k_al=$(key alice)
node() { # name key "extra up args" [lan]
  docker run -d --name "hlra-$1-$sfx" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun -v "hlra-$1-$sfx:/var/lib/tailscale" \
    -e TS_AUTHKEY="$2" -e TS_HOSTNAME="$1" -e TS_USERSPACE=false -e TS_STATE_DIR=/var/lib/tailscale \
    -v "$T/ca/ca.pem:/ca.pem:ro" -e SSL_CERT_FILE=/ca.pem -e TS_EXTRA_ARGS="--login-server=https://$hs $3" "$ts_image" >/dev/null || die "start $1"
  [ -n "${4:-}" ] && docker network connect --ip "$lan.$4" "$lnet" "hlra-$1-$sfx" >/dev/null
  wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list -o json | grep -q '\"given_name\": *\"$1\"'" || die "$1 did not join"
}
# A web server on the private LAN that only the gateway reaches.
docker run -d --name "hlra-lansrv-$sfx" --network "$lnet" --ip "$lan.80" python:3.13-alpine \
  python3 -c 'import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s): b=b"lan-ok"; s.send_response(200); s.send_header("Content-Length","6"); s.end_headers(); s.wfile.write(b)
    def log_message(s,*a): pass
h.ThreadingHTTPServer(("0.0.0.0",8000),E).serve_forever()' >/dev/null || die lansrv
# gateway: /32 direct-host routes (the exit-node pattern) and a private subnet
node gw "$k_gw" "--advertise-routes=203.0.113.10/32,198.51.100.7/32,$lan.0/24" 20
node ex "$k_ex" "--advertise-exit-node"
# a user's node that advertises a route: must NOT be auto-approved
node rogue "$k_al" "--advertise-routes=192.0.2.0/24"
node client "$k_al" "--accept-routes"

routes() { # name -> approved routes, sorted, comma-joined
  hx nodes list -o json | python3 -c "
import json,sys
n=[x for x in json.load(sys.stdin) if x['given_name']=='$1'][0]
print(','.join(sorted(n.get('approved_routes') or [])))"
}
expect_routes() { # name want label
  got=""; i=0; while [ $i -lt 30 ]; do got=$(routes "$1"); [ "$got" = "$2" ] && break; i=$((i + 1)); sleep 1; done
  if [ "$got" = "$2" ]; then ok "$3"; else nok "$3: want [$2], got [$got]"; fi
}
check_all() { # phase [gateway routes]
  p=$1
  want_gw=${2:-$(printf '%s\n' 198.51.100.7/32 203.0.113.10/32 "$lan.0/24" | sort | paste -sd, -)}
  expect_routes gw "$want_gw" "$p: gateway /32 and subnet routes approved by policy"
  expect_routes ex "0.0.0.0/0,::/0" "$p: exit node approved by policy"
  expect_routes rogue "" "$p: a user's node is NOT approved"
  # the peer actually routes through the gateway to the private LAN
  if wait_for 40 docker exec "hlra-client-$sfx" sh -c "wget -qO- -T 4 http://$lan.80:8000/ | grep -qx lan-ok"; then
    ok "$p: a peer reaches the private LAN through the approved subnet route"
  else nok "$p: a peer cannot reach the LAN through the gateway"; fi
  docker exec "hlra-client-$sfx" tailscale exit-node list 2>/dev/null | grep -q " ex " \
    && ok "$p: the exit node is offered to the peer" || nok "$p: the exit node is not offered"
  docker exec "hlra-client-$sfx" sh -c "ip route show table 52 2>/dev/null | grep -q 203.0.113.10" \
    && ok "$p: the peer installed a /32 route through the gateway" || nok "$p: no /32 route at the peer"
}
check_all "$(hx version | head -1 | awk '{print $3}')"

# A route that is outside every prefix (none is: the halves cover all IPv4) - instead test IPv6, which the policy does
# NOT cover: it must stay unapproved. Then a route added later must be approved without a restart.
docker exec "hlra-gw-$sfx" tailscale set --advertise-routes="203.0.113.10/32,198.51.100.7/32,$lan.0/24,203.0.113.99/32,2001:db8::/48" >/dev/null 2>&1
expect_routes gw "$(printf '%s\n' 198.51.100.7/32 203.0.113.10/32 203.0.113.99/32 "$lan.0/24" | sort | paste -sd, -)" \
  "a /32 added later is approved without a restart; IPv6 outside the policy is not"

if [ -n "$up" ]; then
  echo "# upgrade in place: $img -> $up"
  docker rm -f "$hs" >/dev/null
  start_hs "$up"
  echo "# headscale: $(hx version 2>/dev/null | head -1)"
  docker logs "$hs" 2>&1 | grep -iE " (ERR|FTL) " | head -3
  # every client reconnects to the new server with its stored identity
  for n in gw ex rogue client; do docker restart "hlra-$n-$sfx" >/dev/null; done
  sleep 10
  check_all "after upgrade" "$(printf '%s\n' 198.51.100.7/32 203.0.113.10/32 203.0.113.99/32 "$lan.0/24" | sort | paste -sd, -)"
  # the clients really talk to the new server: each one is online on it
  online=$(hx nodes list -o json | python3 -c "import json,sys;print(sum(1 for n in json.load(sys.stdin) if n.get('online')))")
  [ "$online" = 4 ] && ok "after upgrade: all 4 nodes online on the new server" || nok "after upgrade: $online/4 nodes online"
  expect_routes gw "$(printf '%s\n' 198.51.100.7/32 203.0.113.10/32 203.0.113.99/32 "$lan.0/24" | sort | paste -sd, -)" \
    "after upgrade: the later /32 is still approved"
  docker exec "hlra-gw-$sfx" tailscale set --advertise-routes="203.0.113.10/32,198.51.100.7/32,$lan.0/24,203.0.113.99/32,203.0.113.123/32" >/dev/null 2>&1
  expect_routes gw "$(printf '%s\n' 198.51.100.7/32 203.0.113.10/32 203.0.113.99/32 203.0.113.123/32 "$lan.0/24" | sort | paste -sd, -)" \
    "after upgrade: a new /32 is approved by policy"
fi
echo "# $pass passed, $fails failed"
[ "$fails" = 0 ]

#!/bin/sh
# On-demand end-to-end test of the bridge rules of examples/compose/control/config/acl.json (#259), on real headscale
# and tailscale. It runs `make test-bridge-policy-e2e`. Needs docker, jq and python3.
#
#   - headscale loads the example policy, and `headscale policy check` passes its tests block with real tagged nodes;
#   - a bridge's userspace SOCKS node (tag:ingress) uses an exit node (tag:exit) and reaches a web server behind it;
#   - without the autogroup:internet rule the same request fails (the fault of #259);
#   - node-control (tag:node-control) reaches the bridge agent port, and an exit node does not.
#
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
set -u
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
policy="$root/examples/compose/control/config/acl.json"

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
command -v jq >/dev/null 2>&1 || skip "jq is not installed"
command -v python3 >/dev/null 2>&1 || skip "python3 is not installed"
tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }
for i in "$hs_image" "$ts_image" python:3.13-alpine; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 600 docker pull -q "$i" >/dev/null 2>&1 || skip "cannot get image $i"
done

T=$(mktemp -d)
sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="bp-e2e-$sfx"; far="bp-e2e-far-$sfx"
hs="bp-e2e-hs-$sfx"; ex="bp-e2e-exit-$sfx"; br="bp-e2e-bridge-$sfx"; nc="bp-e2e-nc-$sfx"; web="bp-e2e-web-$sfx"
cleanup() {
  for c in "$hs" "$ex" "$br" "$nc" "$web"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" "$far" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $*"; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

# Networks: "net" is the tailnet side, "far" stands for the internet behind the exit node. The bridge is only on "net", so
# it reaches the web server only through the exit node.
dk network create "$net" >/dev/null || die "network"
dk network create --subnet 198.51.100.0/24 "$far" >/dev/null || die "far network"
dk run -d --name "$web" --network "$far" --ip 198.51.100.10 python:3.13-alpine python3 -m http.server 8000 >/dev/null || die "web"

mkdir -p "$T/etc"
cat > "$T/etc/config.yaml" <<YAML
server_url: http://$hs:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise: {private_key_path: /data/noise.key}
prefixes: {v4: 100.64.0.0/10, v6: fd7a:115c:a1e0::/48}
derp:
  server: {enabled: true, region_id: 999, region_code: e2e, region_name: e2e, stun_listen_addr: "0.0.0.0:3478",
           private_key_path: /data/derp.key, automatically_add_embedded_derp_region: true}
  urls: []
  auto_update_enabled: false
database: {type: sqlite, sqlite: {path: /data/db.sqlite}}
dns: {magic_dns: false, override_local_dns: false}
policy: {mode: file, path: /etc/headscale/acl.json}
unix_socket: /data/hs.sock
log: {level: info}
YAML
cp "$policy" "$T/etc/acl.json"
chmod 644 "$T/etc/"*
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale" "$hs_image" serve >/dev/null || die "headscale"
hsx() { dk exec "$hs" headscale "$@"; }
wait_for 40 hsx users list || die "headscale not ready: $(docker logs --tail 5 "$hs" 2>&1)"
ok "headscale starts with the example policy"
hsx users create _svcs >/dev/null || die "user"
uid=$(hsx users list -o json | jq -r '.[] | select(.name == "_svcs") | .id')
key() { hsx preauthkeys create -u "$uid" -e 1h --tags "$1" -o json | jq -r .key; }

# The exit node also reaches "far", like a real exit node reaches the internet through its gateway.
dk run -d --name "$ex" --network "$net" -e TS_AUTHKEY="$(key tag:exit)" \
  -e TS_USERSPACE=true -e TS_HOSTNAME=exit1 -e TS_STATE_DIR=/var/lib/ts \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --advertise-exit-node" "$ts_image" >/dev/null || die "exit"
dk network connect "$far" "$ex" >/dev/null || die "exit far"
# The bridge's SOCKS node: userspace tailscaled with a SOCKS5 port, as tailscale-userspace-socks runs it.
dk run -d --name "$br" --network "$net" -e TS_AUTHKEY="$(key tag:ingress)" -e TS_USERSPACE=true -e TS_HOSTNAME=bridge1-socks \
  -e TS_SOCKS5_SERVER=127.0.0.1:1055 -e TS_STATE_DIR=/var/lib/ts \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" "$ts_image" >/dev/null || die "bridge"
# node-control: a plain node with its tag; a listener on 9000 on the bridge stands in for the agent.
dk run -d --name "$nc" --network "$net" -e TS_AUTHKEY="$(key tag:node-control)" -e TS_USERSPACE=false --cap-add NET_ADMIN \
  --device /dev/net/tun -e TS_HOSTNAME=node-control -e TS_STATE_DIR=/var/lib/ts \
  -e TS_EXTRA_ARGS="--login-server=http://$hs:8080" "$ts_image" >/dev/null || die "node-control"
online() { [ "$(hsx nodes list -o json | jq --arg n "$1" '[.[] | select(.given_name == $n and .online)] | length')" = 1 ]; }
for n in exit1 bridge1-socks node-control; do wait_for 60 online "$n" || die "$n did not join"; done
# An exit node does not forward to a network it is attached to unless it also advertises it (as in the hysteria
# e2e). "far" uses a public test range (TEST-NET-2), because autogroup:internet does not cover private addresses.
dk exec "$ex" tailscale set --advertise-exit-node --advertise-routes=198.51.100.0/24 >/dev/null || die "exit routes"
exid=$(hsx nodes list -o json | jq -r '.[] | select(.given_name == "exit1") | .id')
wait_for 20 hsx nodes approve-routes -i "$exid" -r 0.0.0.0/0,::/0,198.51.100.0/24 || die "approve exit routes"
ok "exit node, bridge SOCKS node and node-control joined with their tags"

echo "# headscale policy check with real tagged nodes"
if hsx policy check -f /etc/headscale/acl.json >"$T/check.log" 2>&1; then ok "the tests block of the example policy passes"
else nok "policy check: $(tail -3 "$T/check.log")"; fi

echo "# the bridge reaches the internet through its exit node"
exit_ip=$(hsx nodes list -o json | jq -r '.[] | select(.given_name == "exit1") | .ip_addresses[0]')
wait_for 30 dk exec "$br" tailscale set --exit-node="$exit_ip" || die "set exit node"
curl_via() { dk exec "$br" sh -c 'command -v curl >/dev/null || apk add -q curl >/dev/null 2>&1; curl -fsS --max-time 6 --socks5 127.0.0.1:1055 http://198.51.100.10:8000/' >/dev/null 2>&1; }
if wait_for 45 curl_via; then ok "with autogroup:internet the SOCKS request reaches the web server"
else nok "the SOCKS request does not get through: $(docker logs --tail 5 "$br" 2>&1 | tr '\n' ' ')"; fi

echo "# node-control reaches the agent port of the bridge; the exit node does not"
dk exec -d "$br" sh -c 'while true; do printf "HTTP/1.0 200 OK\r\n\r\nagent" | nc -l -p 9000; done' >/dev/null 2>&1
br_ip=$(hsx nodes list -o json | jq -r '.[] | select(.given_name == "bridge1-socks") | .ip_addresses[0]')
reach() { dk exec "$1" sh -c "printf 'GET / HTTP/1.0\r\n\r\n' | nc -w 4 $br_ip 9000" 2>/dev/null | grep -q agent; }
if wait_for 20 reach "$nc"; then ok "node-control -> bridge:9000 is allowed"; else nok "node-control does not reach bridge:9000"; fi
if reach "$ex"; then nok "the exit node reaches bridge:9000"; else ok "exit node -> bridge:9000 is denied"; fi

echo "# without autogroup:internet the bridge gets nothing back (#259)"
python3 - "$T/etc/acl.json" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s2 = re.sub(r'\n\s*\{"action": "accept", "src": \["tag:ingress"\], "dst": \["autogroup:internet:\*"\]\},', "", s)
assert s2 != s, "the autogroup:internet rule was not found"
open(p, "w").write(s2)
PY
# SIGHUP reloads the policy file. A restart would give headscale a new address on the network. headscale 0.29.7
# sometimes logs the SIGHUP and does not reload; a second SIGHUP then works. So send it until the log confirms.
reloaded() { [ "$(docker logs "$hs" 2>&1 | grep -c 'Policy reload completed')" -gt 0 ]; }
i=0; while [ $i -lt 5 ] && ! reloaded; do dk kill -s HUP "$hs" >/dev/null || die "reload headscale"; sleep 3; i=$((i + 1)); done
reloaded || nok "headscale did not reload the policy after $i SIGHUP"
# The exit node's packet filter for the bridge: with autogroup:internet it lists the public ranges (dozens of
# prefixes), without it only tailnet addresses.
internet_prefixes() { dk exec "$ex" tailscale debug netmap | jq '[.PacketFilterRules[].DstPorts[].IP | select(startswith("100.") or startswith("fd7a:") | not)] | length'; }
no_internet() { [ "$(internet_prefixes)" = 0 ]; }
wait_for 30 no_internet || nok "the exit node did not get the new policy ($(internet_prefixes) internet prefixes)"
# An open connection or a cached answer can last a moment after the filter changes: wait until it fails.
fails_now() { ! curl_via; }
if wait_for 30 fails_now; then ok "without autogroup:internet the SOCKS request fails, as on the live site"
else nok "the bridge still reaches the web server without autogroup:internet"; fi

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

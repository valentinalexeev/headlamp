#!/bin/sh
# On-demand smoke test of examples/compose/control (#41) on the local docker. Run it with `make test-compose-control`.
#
# It runs the real compose.yaml with an override: images built from this tree, Caddy's internal CA instead of
# Let's Encrypt, a temporary data directory, and the edge on a free local port. It then checks:
#   - headscale answers over HTTPS through the edge;
#   - node-control joins the tailnet through the public URL;
#   - a tailscale client joins through the edge and reaches node-control over the tailnet;
#   - the edge refuses an unknown name.
set -u
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
src="$root/examples/compose/control"
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
domain=headscale.compose.test

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
docker compose version >/dev/null 2>&1 || skip "docker compose is not available"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
T=$(mktemp -d)
proj="hl-compose-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
dc() { tmo 300 docker compose -p "$proj" --project-directory "$T/app" -f "$T/app/compose.yaml" -f "$T/override.yaml" "$@"; }
cl="$proj-client"
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: project $proj in $T"; return; fi
  docker rm -f "$cl" >/dev/null 2>&1
  dc down -v --remove-orphans >/dev/null 2>&1
  docker run --rm -v "$T:/t" alpine:3.22 rm -rf /t/data >/dev/null 2>&1
  rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; dc logs --tail 30 2>&1 | tail -80; docker logs --tail 20 "$cl" 2>&1; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

# the example as a user copies it, with the placeholders filled in
cp -R "$src" "$T/app"
mkdir -p "$T/data"
sed -e "s#^server_url: .*#server_url: https://$domain#" "$src/config/headscale.yaml" > "$T/app/config/headscale.yaml"
printf '%s\n' '{"tagOwners":{"tag:node-control":["e2e@"]},"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' \
  > "$T/app/config/acl.json"
cat > "$T/app/.env" <<EOF
HEADSCALE_IMAGE=hl-compose-headscale:local
NODE_CONTROL_IMAGE=hl-compose-node-control:local
HEADSCALE_DOMAIN=$domain
ACME_EMAIL=test@example.com
ENROLL_DOMAIN=enroll.compose.test
DATA_DIR=$T/data
EDGE_SUBNET=172.30.77.0/24
EOF
: > "$T/app/node-control.env"

# The test differs from production only here: the edge (the default build, no DNS plugin) uses Caddy's internal CA,
# the images are local, and every
# container resolves the public name to the edge, as DNS would.
cat > "$T/override.yaml" <<EOF
services:
  edge:
    build: !reset null
    image: hl-compose-edge-ondemand:local
    ports: !override
      - "127.0.0.1::443"
    volumes: !override
      - ./Caddyfile.test:/etc/caddy/Caddyfile:ro
      - ./caddy/enroll-paths.caddy:/etc/caddy/enroll-paths.caddy:ro
      - ./caddy/dns:/etc/caddy/dns:ro
      - ./caddy/tls:/etc/caddy/tls:ro
      - $T/data/caddy:/data
    networks:
      default:
        ipv4_address: 172.30.77.10
  headscale:
    extra_hosts: ["$domain:172.30.77.10"]
  node-control:
    extra_hosts: ["$domain:172.30.77.10"]
    environment:
      SSL_CERT_FILE: /ca/root.crt
    volumes:
      - $T/ca:/ca:ro
EOF
mkdir -p "$T/ca"
# the production Caddyfile with the ACME issuer swapped for the internal CA (the global options block only). The
# enrolment name then uses tls/acme.caddy, which local_certs also covers. The
# funnel block stays: with the default FUNNEL_DNS=ondemand it runs on the plain image.
awk '/^\{$/ && !done { print; print "\tlocal_certs"; skip = 1; next } skip && /^\}$/ { skip = 0; done = 1 } !skip { print }' \
  "$src/caddy/Caddyfile" > "$T/app/Caddyfile.test"

echo "# building images"
tmo 600 docker build -q -t hl-compose-headscale:local "$root/apps/headscale" >/dev/null || die "headscale image"
tmo 600 docker build -q -t hl-compose-node-control:local "$root/apps/node-control" >/dev/null || die "node-control image"
docker image inspect "$ts_image" >/dev/null 2>&1 || tmo 300 docker pull -q "$ts_image" >/dev/null || die "pull $ts_image"
# The production edge image: the default build (no plugin) and each documented DNS provider build, each with the
# full Caddyfile (funnel block included) for its FUNNEL_DNS value.
for v in "ondemand|" "none|" "route53|github.com/caddy-dns/route53@v1.6.0" "googleclouddns|github.com/caddy-dns/googleclouddns@v1.1.0" "rfc2136|github.com/caddy-dns/rfc2136@v1.0.0"; do
  dns=${v%%|*}; mods=${v#*|}
  if tmo 900 docker build -q --build-arg DNS_PLUGINS="$mods" -t "hl-compose-edge-$dns:local" "$src/caddy" >/dev/null 2>&1 && \
     docker run --rm -e HEADSCALE_DOMAIN=h.example.test -e ACME_EMAIL=a@example.test -e FUNNEL_DOMAIN=f.example.test \
       -e FUNNEL_DNS="$dns" -e GCP_PROJECT=demo-project -e RFC2136_KEY_NAME=k -e RFC2136_KEY_ALG=hmac-sha256 -e RFC2136_KEY=YQ== -e RFC2136_SERVER=192.0.2.1:53 \
       -v "$src/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$src/caddy/dns:/etc/caddy/dns:ro" \
       -v "$src/caddy/tls:/etc/caddy/tls:ro" -v "$src/caddy/enroll-paths.caddy:/etc/caddy/enroll-paths.caddy:ro" \
       "hl-compose-edge-$dns:local" caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
    ok "the edge builds and validates with FUNNEL_DNS=$dns"
  else nok "the edge builds and validates with FUNNEL_DNS=$dns"; fi
done

dc config -q || die "compose config"
ok "compose.yaml is valid with the example settings"

echo "# headscale and the edge"
dc up -d headscale edge >/dev/null 2>&1 || die "up headscale edge"
wait_for 60 sh -c "docker compose -p $proj ps --format '{{.Health}}' headscale | grep -q healthy" || die "headscale not healthy"
edge_port=$(docker compose -p "$proj" port edge 443 | sed 's/.*://')
wait_for 30 dc cp edge:/data/caddy/pki/authorities/local/root.crt "$T/ca/root.crt" || die "edge CA"
if wait_for 30 curl -fsS --cacert "$T/ca/root.crt" --resolve "$domain:$edge_port:127.0.0.1" "https://$domain:$edge_port/health"; then
  ok "headscale answers over HTTPS through the edge"
else nok "headscale answers over HTTPS through the edge"; fi
code=$(curl -s -o /dev/null -w '%{http_code}' -k --resolve "other.compose.test:$edge_port:127.0.0.1" "https://other.compose.test:$edge_port/" || true)
case "$code" in 000|404|421) ok "the edge serves no unknown name ($code)";; *) nok "the edge serves no unknown name ($code)";; esac

# #260: the enrolment name uses the shared path file. node-control is not up yet, so a proxied path gets 502 from
# Caddy, and any other path gets the edge's own 404.
en=enroll.compose.test
ecode() { curl -s -o /dev/null -w '%{http_code}' -k --resolve "$en:$edge_port:127.0.0.1" "https://$en:$edge_port$1" || true; }
for p in /sub/abc /sub/abc/stash /me /oidc/login "/i/$(printf 'ab%.0s' $(seq 32))"; do
  c=$(ecode "$p"); [ "$c" = 502 ] && ok "the edge proxies $p (enroll-paths.caddy)" || nok "the edge does not proxy $p ($c)"
done
for p in /admin /me/ /sub/abc/bad; do
  c=$(ecode "$p"); [ "$c" = 404 ] && ok "the edge refuses $p" || nok "the edge passes $p ($c)"
done

hsx() { dc exec -T headscale headscale -c /config/headscale.yaml "$@"; }
hsx users create e2e >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_cl=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
printf 'TAILSCALE_AUTH_KEY=%s\nHEADSCALE_API_KEY=%s\nADMIN_USERS=e2e\n' "$k_nc" "$api" > "$T/app/node-control.env"

echo "# node-control"
dc up -d node-control >/dev/null 2>&1 || die "up node-control"
if wait_for 60 sh -c "docker compose -p $proj exec -T headscale headscale -c /config/headscale.yaml nodes list | grep -q node-control"; then
  ok "node-control joined through https://$domain"
else nok "node-control joined through https://$domain"; fi

echo "# a client through the edge"
net=$(docker network ls -q --filter "name=${proj}_default")
docker run -d --name "$cl" --network "$net" --add-host "$domain:172.30.77.10" -v "$T/ca:/ca:ro" -e SSL_CERT_FILE=/ca/root.crt \
  -e TS_AUTHKEY="$k_cl" -e TS_EXTRA_ARGS="--login-server=https://$domain" -e TS_HOSTNAME=client -e TS_USERSPACE=true \
  "$ts_image" >/dev/null || die "client"
if wait_for 60 sh -c "docker exec $cl tailscale status 2>/dev/null | grep -q node-control"; then
  ok "a client joined through the edge and sees node-control"
else nok "a client joined through the edge and sees node-control"; fi
if wait_for 30 docker exec "$cl" tailscale ping -c 1 node-control; then ok "the client reaches node-control over the tailnet"
else nok "the client reaches node-control over the tailnet"; fi

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

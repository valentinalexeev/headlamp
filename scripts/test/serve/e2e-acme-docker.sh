#!/bin/sh
# shellcheck disable=SC2015 # ok/nok always succeed
# On-demand end-to-end test of serve certificates from an ACME CA (#334). Run it with `make test-serve-acme-e2e`.
# Needs docker and openssl. It builds the node-control and funnel-sidecar images from this tree.
#
# On one docker network: an authoritative DNS server for the serve zone that takes RFC 2136 updates signed with a
# TSIG key (BIND), the ACME test server (Pebble) that checks DNS-01 against that DNS server, headscale (the serve zone
# split to node-control), node-control with SERVE_CERT_SOURCE=acme and SERVE_ACME_DNS=rfc2136, a plain HTTP backend,
# two funnel-sidecars in serve mode, and a tailscale client. The test checks that node-control gets a wildcard
# certificate through DNS-01, that the sidecars serve it, and that the client sees a chain to the Pebble root on the
# serve names, with no serve CA installed.
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
#   E2E_PEBBLE_IMAGE     default ghcr.io/letsencrypt/pebble:2.7.0
#   E2E_KEEP=1           keep the containers on exit, for debugging
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
pebble_image=${E2E_PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:2.7.0}
base=tailnet.e2e.test
zone=svc.e2e.example

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v openssl >/dev/null 2>&1 || skip "openssl is not available"
tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
sub="172.30.$oct"; DNS_IP="$sub.53"
net="sacme-$sfx"
hs="sacme-hs-$sfx"; nc="sacme-nc-$sfx"; cl="sacme-cl-$sfx"; be="sacme-be-$sfx"; sc1="sacme-sc1-$sfx"; sc2="sacme-sc2-$sfx"
dns="sacme-dns-$sfx"; acme="sacme-acme-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers sacme-*-$sfx"; return; fi
  for c in "$cl" "$sc1" "$sc2" "$be" "$nc" "$hs" "$acme" "$dns"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$dns" "$acme" "$nc" "$sc1" "$sc2"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# building images"
dk build -q -t serve-e2e-node-control "$root/apps/node-control" >/dev/null || die "node-control image"
dk build -q -f "$root/apps/funnel-ingress/Dockerfile.sidecar" -t serve-e2e-sidecar "$root/apps/funnel-ingress" >/dev/null || die "sidecar image"
# BIND from alpine (every architecture); the ISC image is amd64 only
printf 'FROM alpine:3.22\nRUN apk add --no-cache bind bind-tools\n' | tmo 300 docker build -q -t sacme-bind - >/dev/null || die "bind image"
for i in "$hs_image" "$ts_image" "$pebble_image" python:3.13-alpine; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done
dk network create --subnet "$sub.0/24" "$net" >/dev/null || die "network $sub.0/24"
mkdir -p "$T/etc" "$T/dns" "$T/acme"

echo "# DNS: BIND, authoritative for $zone, RFC 2136 updates with a TSIG key"
tsig=$(head -c 32 /dev/urandom | base64)
cat > "$T/dns/named.conf" <<CONF
options { directory "/var/cache/bind"; listen-on { any; }; listen-on-v6 { none; }; recursion no; allow-query { any; }; dnssec-validation no; };
key "e2e-key." { algorithm hmac-sha256; secret "$tsig"; };
zone "$zone" { type primary; file "/var/lib/bind/zone.db"; update-policy { grant e2e-key. zonesub TXT; }; };
CONF
# The NS name is the container name, so every part (lego, Pebble) can reach the primary by name.
cat > "$T/dns/zone.db" <<ZONE
\$TTL 60
@ IN SOA $dns. hostmaster.$zone. 1 60 60 600 60
@ IN NS $dns.
ZONE
chmod 644 "$T/dns/"*
# named writes a journal next to the zone file, so the file is copied into the container, not mounted
dk run -d --name "$dns" --network "$net" --ip "$DNS_IP" -v "$T/dns:/zone:ro" -v "$T/dns/named.conf:/etc/bind/named.conf:ro" \
  sacme-bind sh -c "mkdir -p /var/lib/bind /var/cache/bind && cp /zone/zone.db /var/lib/bind/zone.db && chown -R named /var/lib/bind /var/cache/bind && exec named -u named -g -c /etc/bind/named.conf" \
  >/dev/null || die "start dns"
wait_for 30 sh -c "docker exec $dns dig +short @127.0.0.1 $zone SOA | grep -q hostmaster" || die "dns not ready"
ok "the DNS server answers for $zone"

echo "# ACME server (Pebble), checking DNS-01 at the DNS server"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=$acme" \
  -addext "subjectAltName=DNS:$acme" -keyout "$T/acme/key.pem" -out "$T/acme/cert.pem" >/dev/null 2>&1 || die "acme cert"
cat > "$T/acme/pebble.json" <<JSON
{"pebble":{"listenAddress":"0.0.0.0:14000","managementListenAddress":"0.0.0.0:15000",
 "certificate":"/acme/cert.pem","privateKey":"/acme/key.pem","httpPort":80,"tlsPort":443,
 "ocspResponderURL":"","externalAccountBindingRequired":false}}
JSON
chmod 644 "$T/acme/"*
dk run -d --name "$acme" --network "$net" -v "$T/acme:/acme:ro" -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
  "$pebble_image" -config /acme/pebble.json -dnsserver "$DNS_IP:53" >/dev/null || die pebble
probe() { docker run --rm --network "$net" python:3.13-alpine sh -c "$1"; }
wait_for 30 probe "wget -q --no-check-certificate -O /dev/null https://$acme:14000/dir" || die "pebble not ready"
ok "the ACME server is up"

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

echo "# headscale"
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" "$hs_image" serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e --email e2e@example.test >/dev/null || die "user"
uid=$(hsx users list -o json | sed -n 's/.*"id": *\([0-9]*\).*/\1/p' | head -1)
key() { hsx preauthkeys create -u "$uid" -e 1h ${1:+--tags "$1"} -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1; }
k_nc=$(key tag:node-control); k_cl=$(key ""); api=$(hsx apikeys create -e 1h 2>/dev/null | tail -1)
[ -n "$k_nc" ] && [ -n "$k_cl" ] && [ -n "$api" ] || die "keys"

echo "# node-control: SERVE_CERT_SOURCE=acme, DNS-01 through RFC 2136"
dk run -d --name "$nc" --network "$net" -v "$T/acme/cert.pem:/acme/ca.pem:ro" \
  -e HEADSCALE_LOGIN_SERVER="http://$hs:8080" -e HEADSCALE_API_KEY="$api" -e TAILSCALE_AUTH_KEY="$k_nc" \
  -e TAILNET_HOSTNAME=node-control -e ADMIN_USERS=e2e@example.test \
  -e SERVE_DB=/tmp/serve.db -e SERVE_ZONE="$zone" -e SERVE_BASE_DOMAIN="$base" -e SERVE_USER=e2e -e SERVE_DNS_REFRESH=3s \
  -e NODE_CONTROL_STATE_KEY="$(head -c 32 /dev/urandom | base64)" \
  -e SERVE_CERT_SOURCE=acme -e SERVE_ACME_DIRECTORY="https://$acme:14000/dir" -e SERVE_ACME_EMAIL=ops@example.com \
  -e SERVE_ACME_DNS=rfc2136 -e SERVE_ACME_RESOLVERS="$DNS_IP:53" -e LEGO_CA_CERTIFICATES=/acme/ca.pem \
  -e RFC2136_NAMESERVER="$DNS_IP:53" -e RFC2136_TSIG_KEY=e2e-key. -e RFC2136_TSIG_SECRET="$tsig" \
  -e RFC2136_TSIG_ALGORITHM=hmac-sha256. -e RFC2136_PROPAGATION_TIMEOUT=60 -e RFC2136_POLLING_INTERVAL=2 -e RFC2136_SEQUENCE_INTERVAL=1 \
  serve-e2e-node-control >/dev/null || die "start node-control"
wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list | grep -q node-control" || die "node-control did not join"
wait_for 90 sh -c "docker logs $nc 2>&1 | grep -q 'serve acme: certificate in place'" \
  && ok "node-control got a certificate for *.$zone through DNS-01 (RFC 2136)" || die "no ACME certificate"
docker logs "$nc" 2>&1 | grep -q "Trying to solve DNS-01" && docker logs "$nc" 2>&1 | grep -q "The server validated our request" \
  && ok "the ACME server validated the DNS-01 challenge" || nok "DNS-01 validation in the node-control log"

echo "# client"
dk run -d --name "$cl" --network "$net" --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY="$k_cl" -e TS_EXTRA_ARGS="--login-server=http://$hs:8080 --accept-dns=true" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false "$ts_image" >/dev/null || die "start client"
dk exec "$cl" apk add --no-cache -q curl bind-tools openssl >/dev/null 2>&1 || die "client tools"
wait_for 60 docker exec "$cl" curl -fsS --max-time 5 "http://100.64.0.1/serve/state" -o /dev/null || die "admin listener"
code=$(docker exec "$cl" curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://100.64.0.1/serve/ca.crt")
[ "$code" = 404 ] && ok "no serve CA to install with acme (/serve/ca.crt is 404)" || nok "/serve/ca.crt answered $code"
# the Pebble root the client trusts, and nothing else
probe "wget -q --no-check-certificate -O - https://$acme:15000/roots/0" > "$T/root.pem" 2>/dev/null
grep -q BEGIN "$T/root.pem" || die "pebble root"
docker cp "$T/root.pem" "$cl:/tmp/root.pem" >/dev/null || die "copy root"

api_post() { docker exec "$cl" curl -fsS --max-time 20 -X POST -H 'Content-Type: application/json' -d "$2" "http://100.64.0.1$1"; }
grafana=$(api_post /serve/services '{"name":"grafana","namespace":"mon"}') || die "create grafana"
apps=$(api_post /serve/services '{"name":"*.apps","namespace":"k8s"}') || die "create *.apps"
gk=$(echo "$grafana" | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p'); ak=$(echo "$apps" | sed -n 's/.*"auth_key":"\([^"]*\)".*/\1/p')
[ -n "$gk" ] && [ -n "$ak" ] || die "no keys returned"
# *.apps needs a second wildcard on the certificate: node-control orders again
wait_for 90 sh -c "docker logs $nc 2>&1 | grep 'certificate in place' | grep -q '\*.apps.$zone'" \
  && ok "a new *.apps service makes node-control order *.apps.$zone too" || nok "second order with *.apps.$zone"

echo "# backend and sidecars"
dk run -d --name "$be" --network "$net" python:3.13-alpine sh -c "mkdir -p /w && echo serve-acme-ok > /w/index.html && exec python3 -m http.server 3000 -d /w" >/dev/null || die backend
sidecar() { dk run -d --name "$1" --network "$net" -e FUNNEL_CONTROL_URL="http://$hs:8080" -e TS_AUTHKEY="$2" \
  -e FUNNEL_HOSTNAME="$3" -e FUNNEL_SERVICES="web=443@$be:3000" -e FUNNEL_SERVE_URL=http://100.64.0.1 serve-e2e-sidecar >/dev/null || die "sidecar $1"; }
sidecar "$sc1" "$gk" grafana; sidecar "$sc2" "$ak" ingress
wait_for 120 sh -c "docker logs $sc1 2>&1 | grep -q 'certificate for grafana.$zone'" && ok "the grafana sidecar got the shared certificate" || nok "grafana sidecar certificate ($(docker logs --tail 3 "$sc1" 2>&1 | tr '\n' ' '))"
wait_for 120 sh -c "docker logs $sc2 2>&1 | grep -q 'certificate for \*.apps.$zone'" && ok "the ingress sidecar got the shared certificate" || nok "ingress sidecar certificate ($(docker logs --tail 3 "$sc2" 2>&1 | tr '\n' ' '))"

echo "# HTTPS from the client, trusting only the Pebble root"
https() { docker exec "$cl" curl -fsS --max-time 10 --cacert /tmp/root.pem "https://$1/"; }
for h in "grafana.$zone" "a.apps.$zone"; do
  body=""; i=0; while [ $i -lt 30 ]; do body=$(https "$h" 2>/dev/null) && break; i=$((i + 1)); sleep 2; done
  [ "$body" = serve-acme-ok ] && ok "HTTPS to $h verifies against the Pebble root and reaches the backend" || nok "HTTPS $h ($(https "$h" 2>&1 | tail -1))"
done
chain=$(docker exec "$cl" sh -c "echo | openssl s_client -connect grafana.$zone:443 -servername grafana.$zone -CAfile /tmp/root.pem 2>/dev/null")
echo "$chain" | grep -q 'Verify return code: 0' && echo "$chain" | grep -qi 'i:.*Pebble' \
  && ok "the chain on grafana.$zone is issued by Pebble and verifies to its root" || nok "chain ($(echo "$chain" | grep -E 'i:|Verify' | tr '\n' ' '))"
if docker exec "$cl" curl -fsS --max-time 10 "https://grafana.$zone/" >/dev/null 2>&1; then nok "without the Pebble root the name must not verify"
else ok "without the Pebble root the name does not verify (the trust comes from the ACME CA)"; fi
# the challenge record is gone after the order
left=$(docker exec "$dns" dig +short @127.0.0.1 "_acme-challenge.$zone" TXT)
[ -z "$left" ] && ok "the _acme-challenge record was cleaned up" || nok "_acme-challenge left: $left"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

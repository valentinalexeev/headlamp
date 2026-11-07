#!/bin/sh
# On-demand end-to-end test of funnel on-demand certificates (#127). Run it with `make test-funnel-tls-e2e`. Needs docker.
#
# A real ACME server (Pebble, Let's Encrypt's test server) issues certificates over HTTP-01. The real edge
# (examples/compose/control/caddy: default build, production Caddyfile, FUNNEL_DNS=ondemand) asks the real
# funnel-ingress before each certificate. funnel-ingress is built from this tree, joins a real headscale, and takes
# its routes from a static file. Only the ACME directory URL and its TLS root differ from production.
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
src="$root/examples/compose/control"
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
pebble_image=${E2E_PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:2.7.0}
domain=funnel.e2e.test

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v openssl >/dev/null 2>&1 || skip "openssl is not available"
tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
oct=$(( $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 200 + 20 ))
sub="172.31.$oct"; EDGE_IP="$sub.10"; DNS_IP="$sub.53"
net="ftls-$sfx"; hs="ftls-hs-$sfx"; fi_="ftls-fi-$sfx"; edge="ftls-edge-$sfx"; acme="ftls-acme-$sfx"; be="ftls-be-$sfx"; dns="ftls-dns-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: ftls-*-$sfx"; return; fi
  for c in "$edge" "$fi_" "$be" "$acme" "$dns" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$edge" "$fi_" "$acme"; do echo "--- logs $c"; docker logs --tail 25 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }
probe() { docker run --rm --network "$net" --dns "$DNS_IP" ftls-client sh -c "$1"; }

echo "# images"
dk build -q -t ftls-funnel-ingress "$root/apps/funnel-ingress" >/dev/null || die "funnel-ingress image"
dk build -q -t ftls-edge "$src/caddy" >/dev/null || die "edge image (default build)"
for i in "$hs_image" "$pebble_image" python:3.13-alpine alpine:3.22; do dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"; done
# The probes run with --dns at the test resolver, which cannot reach a package mirror: build the client tools first.
printf 'FROM alpine:3.22\nRUN apk add --no-cache curl openssl\n' | tmo 300 docker build -q -t ftls-client - >/dev/null || die "client image"
dk network create --subnet "$sub.0/24" "$net" >/dev/null || die "network $sub.0/24"

echo "# headscale"
mkdir -p "$T/hs" "$T/fi" "$T/acme"
cat > "$T/hs/config.yaml" <<YAML
server_url: http://$hs:8080
listen_addr: 0.0.0.0:8080
noise:
  private_key_path: /data/noise.key
prefixes:
  v4: 100.64.0.0/10
derp:
  server:
    enabled: true
    region_id: 999
    region_code: e2e
    region_name: e2e
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
YAML
dk run -d --name "$hs" --network "$net" -v "$T/hs:/etc/headscale:ro" "$hs_image" \
  serve -c /etc/headscale/config.yaml >/dev/null || die headscale
wait_for 40 docker exec "$hs" headscale -c /etc/headscale/config.yaml users list || die "headscale not ready"
hsx() { dk exec -i "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
hsx users create e2e >/dev/null
printf '%s' '{"tagOwners":{"tag:funnel-ingress":["e2e@"]},"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}' | hsx policy set -f /dev/stdin >/dev/null || die policy
key=$(hsx preauthkeys create -u 1 --tags tag:funnel-ingress -e 1h -o json | sed -n 's/.*"key": *"\([^"]*\)".*/\1/p' | head -1)
[ -n "$key" ] || die "auth key"

echo "# backend, and funnel-ingress with one route: admin.pdl.$domain"
dk run -d --name "$be" --network "$net" --ip "$sub.30" python:3.13-alpine \
  sh -c 'mkdir -p /w && echo funnel-ok > /w/index.html && exec python3 -m http.server 3000 -d /w' >/dev/null || die backend
cat > "$T/fi/config.json" <<JSON
{"version":"e2e-1","public_domain":"$domain","namespaces":{"pdl":{"ports":[{"lo":3000,"hi":3000}]}},
 "routes":[{"ns":"pdl","service":"admin","target":"$sub.30","port":3000}],"blocked":[]}
JSON
# The alias is the compose service name, which the production Caddyfile's ask URL uses.
dk run -d --name "$fi_" --network "$net" --network-alias funnel-ingress -v "$T/fi:/cfg:ro" -e TS_AUTHKEY="$key" -e FUNNEL_CONTROL_URL="http://$hs:8080" \
  -e FUNNEL_CONFIG_FILE=/cfg/config.json -e FUNNEL_CLIENT_IP_HEADER=X-Forwarded-For -e FUNNEL_TRUSTED_PROXIES="$EDGE_IP/32" \
  ftls-funnel-ingress >/dev/null || die funnel-ingress
askcode() { probe "wget -S -qO /dev/null 'http://$fi_:8080/.well-known/headlamp/tls-ask?domain=$1' 2>&1 | sed -n 's/^ *HTTP[^ ]* \([0-9]*\).*/\1/p' | tail -1"; }
wait_for 60 sh -c "[ \"\$(docker run --rm --network $net alpine:3.22 sh -c \"wget -S -qO /dev/null 'http://$fi_:8080/.well-known/headlamp/tls-ask?domain=admin.pdl.$domain' 2>&1 | grep -c ' 200 '\")\" -ge 1 ]" \
  || die "funnel-ingress did not start ($(docker logs --tail 5 "$fi_" 2>&1 | tr '\n' ' '))"
ok "funnel-ingress approves admin.pdl.$domain"
[ "$(askcode "nope.pdl.$domain")" = 403 ] && ok "funnel-ingress refuses nope.pdl.$domain (no route)" || nok "funnel-ingress refuses nope.pdl.$domain"
[ "$(askcode "evil.example.org")" = 403 ] && ok "funnel-ingress refuses a name outside the domain" || nok "funnel-ingress refuses evil.example.org"

echo "# a DNS server for the test domain: every name points at the edge (as the public wildcard record would)"
cat > "$T/dns.py" <<PY
# Answers *.$domain with the edge address, as the public wildcard record would. Every other name goes to docker's
# resolver (127.0.0.11 inside this container), so container names such as funnel-ingress still resolve.
import socket, threading
DOMAIN = b"$domain".split(b".")
ip = bytes(int(x) for x in "$EDGE_IP".split("."))
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("0.0.0.0", 53))
def labels(q):
    i, out = 12, []
    while q[i]:
        out.append(q[i+1:i+1+q[i]].lower()); i += q[i] + 1
    return out, i
def handle(q, a):
    name, i = labels(q)
    if name[-len(DOMAIN):] == DOMAIN:
        qtype, end = q[i+1:i+3], i + 5
        an = b"\x00\x01" if qtype == b"\x00\x01" else b"\x00\x00"
        body = q[12:end] + ((b"\xc0\x0c\x00\x01\x00\x01\x00\x00\x00\x3c\x00\x04" + ip) if qtype == b"\x00\x01" else b"")
        s.sendto(q[:2] + b"\x85\x80" + b"\x00\x01" + an + b"\x00\x00\x00\x00" + body, a)
        return
    f = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); f.settimeout(3)
    try:
        f.sendto(q, ("127.0.0.11", 53)); s.sendto(f.recv(4096), a)
    except OSError:
        pass
while True:
    q, a = s.recvfrom(512)
    threading.Thread(target=handle, args=(q, a), daemon=True).start()
PY
dk run -d --name "$dns" --network "$net" --ip "$DNS_IP" -v "$T/dns.py:/dns.py:ro" python:3.13-alpine python3 /dns.py >/dev/null || die dns

echo "# ACME server (Pebble), with a TLS certificate for its container name"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=$acme" \
  -addext "subjectAltName=DNS:$acme" -keyout "$T/acme/key.pem" -out "$T/acme/cert.pem" >/dev/null 2>&1 || die "acme cert"
cat > "$T/acme/pebble.json" <<JSON
{"pebble":{"listenAddress":"0.0.0.0:14000","managementListenAddress":"0.0.0.0:15000",
 "certificate":"/acme/cert.pem","privateKey":"/acme/key.pem","httpPort":80,"tlsPort":443,
 "ocspResponderURL":"","externalAccountBindingRequired":false}}
JSON
chmod 644 "$T/acme/"*
dk run -d --name "$acme" --network "$net" -v "$T/acme:/acme:ro" -e PEBBLE_VA_NOSLEEP=1 \
  "$pebble_image" -config /acme/pebble.json -dnsserver "$DNS_IP:53" >/dev/null || die pebble
wait_for 30 probe "wget -q --no-check-certificate -O /dev/null https://$acme:14000/dir" || die "pebble not ready"
ok "the ACME server is up"

echo "# the edge: the production Caddyfile, with Pebble as the ACME CA"
dk run -d --name "$edge" --network "$net" --ip "$EDGE_IP" --dns "$DNS_IP" \
  -e HEADSCALE_DOMAIN=headscale.invalid -e ACME_EMAIL=e2e@example.test -e FUNNEL_DOMAIN="$domain" -e FUNNEL_DNS=ondemand \
  -e ENROLL_DOMAIN=enroll.invalid -e CADDY_ACME_CA="https://$acme:14000/dir" -e SSL_CERT_FILE=/acme/cert.pem \
  -v "$src/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$src/caddy/dns:/etc/caddy/dns:ro" -v "$T/acme:/acme:ro" \
  ftls-edge >/dev/null || die edge
wait_for 30 probe "nc -z $EDGE_IP 443" || die "edge not listening"

echo "# a client on the internet side"
roots() { probe "wget -q --no-check-certificate -O - https://$acme:15000/roots/0 2>/dev/null; wget -q --no-check-certificate -O - https://$acme:15000/intermediates/0 2>/dev/null"; }
fetch() { # host -> body, verifying against Pebble's issuing roots
  docker run --rm --network "$net" --dns "$DNS_IP" -v "$T/acme:/acme:ro" ftls-client \
    curl -fsS --max-time 20 --cacert /acme/roots.pem "https://$1/"
}
i=0; while [ $i -lt 20 ]; do roots > "$T/acme/roots.pem" 2>/dev/null; grep -q BEGIN "$T/acme/roots.pem" && break; i=$((i + 1)); sleep 1; done
grep -q BEGIN "$T/acme/roots.pem" || die "pebble roots"
body=""; i=0; while [ $i -lt 10 ]; do body=$(fetch "admin.pdl.$domain" 2>/dev/null) && break; i=$((i + 1)); sleep 3; done
[ "$body" = funnel-ok ] && ok "first HTTPS request to admin.pdl.$domain: a certificate from the ACME server, verified, reaches the backend" \
  || nok "HTTPS admin.pdl.$domain ($(fetch "admin.pdl.$domain" 2>&1 | tail -1))"
issuer=$(docker run --rm --network "$net" --dns "$DNS_IP" ftls-client sh -c "echo | openssl s_client -connect $EDGE_IP:443 -servername admin.pdl.$domain 2>/dev/null | openssl x509 -noout -issuer -subject -ext subjectAltName 2>/dev/null" | tr '\n' ' ')
echo "$issuer" | grep -qi pebble && echo "$issuer" | grep -q "DNS:admin.pdl.$domain" && ok "the certificate is for exactly admin.pdl.$domain, issued by the ACME server" || nok "certificate issuer/name ($issuer)"
for h in "nope.pdl.$domain" "admin.other.$domain"; do
  out=$(fetch "$h" 2>&1); rc=$?
  # curl 35 (TLS handshake) or 60 (verify): the edge had no certificate to offer. Anything else is a broken probe.
  case "$rc" in 35|60) ok "$h gets no certificate (curl exit $rc: no route, no issuance)";;
    0) nok "$h must get no certificate";; *) nok "$h probe failed for another reason (exit $rc: $out)";; esac
done
docker logs "$acme" 2>&1 | grep -q "nope.pdl" && nok "the edge asked the ACME server for nope.pdl" || ok "the ACME server never saw an order for a refused host"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ] || die "failures"

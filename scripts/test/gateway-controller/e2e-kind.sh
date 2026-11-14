#!/bin/sh
# shellcheck disable=SC2015 # ok/nok always succeed
# e2e (#338): charts/gateway-controller in a kind cluster against the pinned headscale (HEADSCALE_VERSION). Proves
# that a Gateway of class headlamp-serve and an HTTPRoute publish a ClusterIP Service on the tailnet through one
# funnel-sidecar Deployment, that the Gateway status has Programmed=True and the tailnet address, that a tailnet
# peer reaches the Service at that address, and that a route with a path match gets Accepted=False.
# The serve certificate comes from a Secret (tls.certificateRefs), so no node-control is needed.
# Needs docker, kind, kubectl, helm, go, openssl, python3. Usage: scripts/test/gateway-controller/e2e-kind.sh
# (E2E_KEEP=1 keeps everything)
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_img=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:$(tr -d '[:space:]' < "$root/HEADSCALE_VERSION")}
tsv=$(tr -d '[:space:]' < "$root/TAILSCALE_VERSION")
ctl_img=${E2E_CONTROLLER_IMAGE:-hl-gateway-controller:e2e}
sc_img=${E2E_SIDECAR_IMAGE:-hl-funnel-sidecar:e2e}
srv_img=python:3.13-alpine
cli_img=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
curl_img=curlimages/curl:8.11.1
sfx=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
cl="hlg-$sfx"; hs="hlg-hs-$sfx"; cli="hlg-client-$sfx"; T=$(mktemp -d)
export KUBECONFIG="$T/kubeconfig"
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: cluster $cl, containers hlg-*-$sfx, KUBECONFIG=$KUBECONFIG"; return; fi
  docker rm -f "$hs" "$cli" >/dev/null 2>&1; docker volume rm "hlg-data-$sfx" >/dev/null 2>&1
  kind delete cluster --name "$cl" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() {
  echo "FATAL: $1"; docker logs --tail 20 "$hs" 2>&1
  kubectl -n gwc logs deploy/gc --tail 40 2>&1; kubectl -n apps logs deploy/web-funnel --tail 30 2>&1; exit 1
}
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }
for c in docker kind kubectl helm go openssl python3; do command -v $c >/dev/null || { echo "SKIP: $c not installed"; exit 0; }; done

if [ -z "${E2E_CONTROLLER_IMAGE:-}" ]; then
  docker build -q -t "$ctl_img" "$root/apps/gateway-controller" >/dev/null || die "build controller image"
fi
if [ -z "${E2E_SIDECAR_IMAGE:-}" ]; then
  docker build -q --build-arg TSVERSION="$tsv" -f "$root/apps/funnel-ingress/Dockerfile.sidecar" -t "$sc_img" \
    "$root/apps/funnel-ingress" >/dev/null || die "build sidecar image"
fi
# the Gateway API CRDs (standard channel) of the version in go.mod
gwapi=$(cd "$root/apps/gateway-controller" && go list -m -f '{{.Dir}}' sigs.k8s.io/gateway-api) || die "gateway-api module"
kind create cluster --name "$cl" --wait 120s >/dev/null 2>&1 || die "kind create"
docker pull -q "$srv_img" >/dev/null; docker pull -q "$curl_img" >/dev/null
kind load docker-image --name "$cl" "$ctl_img" "$sc_img" "$srv_img" >/dev/null 2>&1 || die "kind load"
kubectl apply --server-side -f "$gwapi/config/crd/standard/" >/dev/null 2>&1 || die "Gateway API CRDs"

# headscale on the kind network, at a fixed address, with TLS from a throwaway CA
sub=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
ip=$(echo "$sub" | awk -F'[./]' '{print $1"."$2"."$3".251"}')
url="https://$ip"
mkdir -p "$T/etc" "$T/ca"
ec() { openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes "$@"; }
ec -x509 -sha256 -days 1 -subj "/CN=e2e CA" -keyout "$T/ca/ca.key" -out "$T/ca/ca.pem" >/dev/null 2>&1 || die ca
ec -subj "/CN=$ip" -keyout "$T/etc/key.pem" -out "$T/ca/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=IP:%s\n' "$ip" > "$T/ca/ext"
openssl x509 -req -sha256 -in "$T/ca/csr.pem" -CA "$T/ca/ca.pem" -CAkey "$T/ca/ca.key" -CAcreateserial -days 1 \
  -extfile "$T/ca/ext" -out "$T/etc/cert.pem" >/dev/null 2>&1 || die leaf
# the serve certificate of the published name (what cert-manager would put in the Secret)
ec -subj "/CN=web.ts.example.com" -keyout "$T/ca/srv.key" -out "$T/ca/srv.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:web.ts.example.com\n' > "$T/ca/srvext"
openssl x509 -req -sha256 -in "$T/ca/srv.csr" -CA "$T/ca/ca.pem" -CAkey "$T/ca/ca.key" -CAcreateserial -days 1 \
  -extfile "$T/ca/srvext" -out "$T/ca/srv.pem" >/dev/null 2>&1 || die "serve cert"
chmod 644 "$T/etc/"*.pem "$T/ca/ca.pem"
cat > "$T/etc/config.yaml" <<YAML
server_url: $url
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
log:
  level: info
YAML
chmod 644 "$T/etc/config.yaml"
docker run -d --name "$hs" --network kind --ip "$ip" -v "$T/etc:/etc/headscale:ro" -v "hlg-data-$sfx:/data" "$hs_img" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
hx() { docker exec -i "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
wait_for 40 hx users list || die "headscale not ready"
echo "# headscale: $(hx version 2>/dev/null | head -1), sidecar tailscale $tsv"
hx users create _svcs >/dev/null; hx users create alice >/dev/null
cat <<'JSON' | hx policy set -f /dev/stdin >/dev/null || die "policy"
{
  "tagOwners": {"tag:serve": ["_svcs@"], "tag:ns-apps": ["_svcs@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
}
JSON
uid() { hx users list -o json | python3 -c "import json,sys;print([u['id'] for u in json.load(sys.stdin) if u['name']=='$1'][0])"; }
k_gw=$(hx preauthkeys create -u "$(uid _svcs)" -e 1h --reusable --ephemeral --tags tag:serve,tag:ns-apps -o json \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])") || die "gateway key"
k_al=$(hx preauthkeys create -u "$(uid alice)" -e 1h --reusable -o json | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")

# the controller, from the chart
kubectl create ns gwc >/dev/null
repo=${ctl_img%:*}; tag=${ctl_img##*:}; srepo=${sc_img%:*}; stag=${sc_img##*:}
helm install gc "$root/charts/gateway-controller" -n gwc --set controlURL="$url" \
  --set image.repository="$repo" --set image.tag="$tag" --set image.pullPolicy=Never \
  --set sidecarImage.repository="$srepo" --set sidecarImage.tag="$stag" \
  --set caBundle.configMap=tailnet-ca >/dev/null || die "helm install"
kubectl -n gwc rollout status deploy/gc --timeout=120s >/dev/null 2>&1 && ok "the controller Deployment is ready" \
  || die "controller not ready"
wait_for 30 sh -c "kubectl get gatewayclass headlamp-serve -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].status}' | grep -qx True" \
  && ok "GatewayClass headlamp-serve is Accepted" || nok "GatewayClass not accepted"

# a ClusterIP Service in namespace apps, a Gateway and an HTTPRoute
kubectl create ns apps >/dev/null
kubectl -n apps create deployment web --image="$srv_img" --port=8000 -- \
  python3 -c 'import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s): b=b"gw-ok"; s.send_response(200); s.send_header("Content-Length","5"); s.end_headers(); s.wfile.write(b)
    def log_message(s,*a): pass
h.ThreadingHTTPServer(("0.0.0.0",8000),E).serve_forever()' >/dev/null
kubectl -n apps expose deployment web --port=80 --target-port=8000 >/dev/null
svc_ip=$(kubectl -n apps get svc web -o jsonpath='{.spec.clusterIP}')
kubectl -n apps create secret generic web-authkey --from-literal=authkey="$k_gw" >/dev/null
kubectl -n apps create secret tls web-tls --cert="$T/ca/srv.pem" --key="$T/ca/srv.key" >/dev/null
kubectl -n apps create configmap tailnet-ca --from-file=ca.crt="$T/ca/ca.pem" >/dev/null
cat <<'YAML' | kubectl apply -f - >/dev/null || die "Gateway and routes"
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: { name: web, namespace: apps }
spec:
  gatewayClassName: headlamp-serve
  infrastructure:
    parametersRef: { group: "", kind: Secret, name: web-authkey }
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
      hostname: web.ts.example.com
      tls: { mode: Terminate, certificateRefs: [{ name: web-tls }] }
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: web, namespace: apps }
spec:
  parentRefs: [{ name: web }]
  hostnames: [web.ts.example.com]
  rules:
    - backendRefs: [{ name: web, port: 80 }]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: api-only, namespace: apps }
spec:
  parentRefs: [{ name: web }]
  rules:
    - matches: [{ path: { type: PathPrefix, value: /api } }]
      backendRefs: [{ name: web, port: 80 }]
YAML

gwcond() { kubectl -n apps get gateway web -o jsonpath="{.status.conditions[?(@.type==\"$1\")].status}"; }
wait_for 30 sh -c "kubectl -n apps get deploy web-funnel" && ok "the controller made the sidecar Deployment web-funnel" \
  || die "no sidecar Deployment"
fs=$(kubectl -n apps get deploy web-funnel -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FUNNEL_SERVICES")].value}')
[ "$fs" = "web=443@$svc_ip:80" ] && ok "FUNNEL_SERVICES targets the ClusterIP ($fs)" || nok "FUNNEL_SERVICES = $fs"
[ "$(gwcond Accepted)" = True ] && ok "the Gateway is Accepted" || nok "Gateway Accepted = $(gwcond Accepted)"
wait_for 150 sh -c "kubectl -n apps get gateway web -o jsonpath='{.status.conditions[?(@.type==\"Programmed\")].status}' | grep -qx True" \
  && ok "the Gateway is Programmed=True" || die "Gateway not Programmed: $(kubectl -n apps get gateway web -o jsonpath='{.status}')"
addr=$(kubectl -n apps get gateway web -o jsonpath='{.status.addresses[0].value}')
case "$addr" in 100.*) ok "the Gateway status has the tailnet address $addr" ;; *) nok "status address: [$addr]" ;; esac
node_ip=$(hx nodes list -o json | python3 -c "import json,sys;print(' '.join(a for n in json.load(sys.stdin) if n['given_name']=='web' for a in n['ip_addresses']))")
case " $node_ip " in *" $addr "*) ok "the address is the address of the headscale node web" ;; *) nok "headscale node web has [$node_ip]" ;; esac
rc() { kubectl -n apps get httproute "$1" -o jsonpath="{.status.parents[0].conditions[?(@.type==\"Accepted\")].$2}"; }
[ "$(rc web status)" = True ] && ok "the HTTPRoute is Accepted" || nok "HTTPRoute web Accepted = $(rc web status)"
rc web message | grep -q "$addr" && ok "the HTTPRoute status names the tailnet address" || nok "route message: $(rc web message)"
[ "$(rc api-only status)" = False ] && [ "$(rc api-only reason)" = UnsupportedValue ] \
  && ok "a route with a path match is Accepted=False (UnsupportedValue: $(rc api-only message))" \
  || nok "path-match route: $(rc api-only status) $(rc api-only reason)"
kubectl auth can-i get secrets -n apps --as=system:serviceaccount:gwc:gc >/dev/null 2>&1 \
  && nok "the controller may read Secrets" || ok "the controller cannot read Secrets"

# a tailnet peer reaches the Service at the Gateway address, with the serve certificate
docker run -d --name "$cli" --network kind --cap-add NET_ADMIN --device /dev/net/tun \
  -v "$T/ca/ca.pem:/ca.pem:ro" -e SSL_CERT_FILE=/ca.pem -e TS_AUTHKEY="$k_al" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false -e TS_STATE_DIR=/tmp -e TS_EXTRA_ARGS="--login-server=$url" "$cli_img" >/dev/null || die "client"
peer() {
  docker run --rm --network "container:$cli" -v "$T/ca/ca.pem:/ca.pem:ro" "$curl_img" -sS -m 5 --cacert /ca.pem \
    --resolve "web.ts.example.com:443:$addr" https://web.ts.example.com/ 2>/dev/null | grep -qx gw-ok
}
if wait_for 60 peer; then
  ok "a tailnet peer reaches the ClusterIP Service at https://web.ts.example.com ($addr)"
else nok "the peer cannot reach $addr"; docker exec "$cli" tailscale status 2>&1 | head; fi

# deleting the route scales the sidecar to 0 and the Gateway leaves Programmed
kubectl -n apps delete httproute web >/dev/null
wait_for 30 sh -c "[ \"\$(kubectl -n apps get deploy web-funnel -o jsonpath='{.spec.replicas}')\" = 0 ]" \
  && ok "without a route the sidecar has 0 replicas" || nok "replicas after route deletion"
wait_for 30 sh -c "kubectl -n apps get gateway web -o jsonpath='{.status.conditions[?(@.type==\"Programmed\")].status}' | grep -qx False" \
  && ok "without a route the Gateway is Programmed=False" || nok "Programmed after route deletion"
echo "# $pass passed, $fails failed"
[ "$fails" = 0 ]

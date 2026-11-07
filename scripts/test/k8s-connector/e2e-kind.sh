#!/bin/sh
# shellcheck disable=SC2015 # ok/nok always succeed
# e2e (#103): charts/tailnet-connector in a kind cluster against the pinned headscale (HEADSCALE_VERSION). Proves that
# the connector joins with an ephemeral tagged key, keeps its state in the kube Secret, that headscale approves only the
# routes the policy gives the connector tag, and that a tailnet peer reaches a ClusterIP Service through it.
# Needs docker, kind, kubectl, helm. Usage: scripts/test/k8s-connector/e2e-kind.sh  (E2E_KEEP=1 keeps everything)
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
hs_img=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:$(tr -d '[:space:]' < "$root/HEADSCALE_VERSION")}
tsv=$(tr -d '[:space:]' < "$root/TAILSCALE_VERSION")
img=${E2E_CONNECTOR_IMAGE:-hl-tailnet-connector:e2e}
srv_img=python:3.13-alpine
# the tailnet peer: the stock client image, in kernel (TUN) mode
cli_img=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
sfx=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
cl="hlk-$sfx"; hs="hlk-hs-$sfx"; cli="hlk-client-$sfx"; T=$(mktemp -d)
export KUBECONFIG="$T/kubeconfig"
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: cluster $cl, containers hlk-*-$sfx, KUBECONFIG=$KUBECONFIG"; return; fi
  docker rm -f "$hs" "$cli" >/dev/null 2>&1; docker volume rm "hlk-data-$sfx" >/dev/null 2>&1
  kind delete cluster --name "$cl" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; docker logs --tail 20 "$hs" 2>&1; kubectl -n conn logs deploy/tc --tail 30 2>&1; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }
for c in docker kind kubectl helm openssl python3; do command -v $c >/dev/null || { echo "SKIP: $c not installed"; exit 0; }; done

if [ -z "${E2E_CONNECTOR_IMAGE:-}" ]; then
  docker build -q --build-arg TSVERSION="$tsv" -t "$img" "$root/apps/tailnet-connector" >/dev/null || die "build image"
fi
kind create cluster --name "$cl" --wait 120s >/dev/null 2>&1 || die "kind create"
docker pull -q "$srv_img" >/dev/null
kind load docker-image --name "$cl" "$img" "$srv_img" >/dev/null 2>&1 || die "kind load"

# headscale on the kind network, at a fixed address, with TLS from a throwaway CA
sub=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -m1 '\.')
ip=$(echo "$sub" | awk -F'[./]' '{print $1"."$2"."$3".250"}')
url="https://$ip"
mkdir -p "$T/etc" "$T/ca"
openssl req -x509 -sha256 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes -days 1 -subj "/CN=e2e CA" \
  -keyout "$T/ca/ca.key" -out "$T/ca/ca.pem" >/dev/null 2>&1 || die ca
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes -subj "/CN=$ip" -keyout "$T/etc/key.pem" -out "$T/ca/csr.pem" >/dev/null 2>&1
printf 'subjectAltName=IP:%s\n' "$ip" > "$T/ca/ext"
openssl x509 -req -sha256 -in "$T/ca/csr.pem" -CA "$T/ca/ca.pem" -CAkey "$T/ca/ca.key" -CAcreateserial -days 1 -extfile "$T/ca/ext" \
  -out "$T/etc/cert.pem" >/dev/null 2>&1 || die leaf
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
docker run -d --name "$hs" --network kind --ip "$ip" -v "$T/etc:/etc/headscale:ro" -v "hlk-data-$sfx:/data" "$hs_img" \
  serve -c /etc/headscale/config.yaml >/dev/null || die "start headscale"
hx() { docker exec -i "$hs" headscale -c /etc/headscale/config.yaml "$@"; }
wait_for 40 hx users list || die "headscale not ready"
echo "# headscale: $(hx version 2>/dev/null | head -1), connector tailscale $tsv"
hx users create _svcs >/dev/null; hx users create alice >/dev/null
# The README's policy: the connector tag may advertise the Service range only. The Pod range is advertised too, and
# must stay unapproved.
cat <<'JSON' | hx policy set -f /dev/stdin >/dev/null || die "policy"
{
  "tagOwners": {"tag:k8s-connector": ["_svcs@"]},
  "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
  "autoApprovers": {"routes": {"10.96.0.0/12": ["tag:k8s-connector"]}}
}
JSON
uid() { hx users list -o json | python3 -c "import json,sys;print([u['id'] for u in json.load(sys.stdin) if u['name']=='$1'][0])"; }
key() { hx preauthkeys create -u "$(uid "$1")" -e 1h "$@" -o json | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])"; }
k_conn=$(hx preauthkeys create -u "$(uid _svcs)" -e 1h --reusable --ephemeral --tags tag:k8s-connector -o json \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])") || die "connector key"
k_al=$(hx preauthkeys create -u "$(uid alice)" -e 1h --reusable -o json | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")

# an in-cluster Service in the Service range
kubectl create ns conn >/dev/null
kubectl -n conn create deployment web --image="$srv_img" --port=8000 -- \
  python3 -c 'import http.server as h
class E(h.BaseHTTPRequestHandler):
    def do_GET(s): b=b"k8s-ok"; s.send_response(200); s.send_header("Content-Length","6"); s.end_headers(); s.wfile.write(b)
    def log_message(s,*a): pass
h.ThreadingHTTPServer(("0.0.0.0",8000),E).serve_forever()' >/dev/null
kubectl -n conn expose deployment web --port=80 --target-port=8000 >/dev/null
svc_ip=$(kubectl -n conn get svc web -o jsonpath='{.spec.clusterIP}')

# the connector, from the chart
kubectl -n conn create secret generic tailnet-connector-authkey --from-literal=authkey="$k_conn" >/dev/null
kubectl -n conn create configmap tailnet-ca --from-file=ca.crt="$T/ca/ca.pem" >/dev/null
repo=${img%:*}; tag=${img##*:}
helm install tc "$root/charts/tailnet-connector" -n conn --set controlURL="$url" \
  --set image.repository="$repo" --set image.tag="$tag" --set image.pullPolicy=Never \
  --set 'connector.routes={10.96.0.0/12,10.244.0.0/16}' --set connector.hostname=k8s-conn \
  --set caBundle.configMap=tailnet-ca >/dev/null || die "helm install"
kubectl -n conn rollout status deploy/tc --timeout=120s >/dev/null 2>&1 && ok "the connector Deployment is ready" || nok "connector not ready"
kubectl -n conn rollout status deploy/web --timeout=120s >/dev/null 2>&1 || die "web not ready"

nodejson() { hx nodes list -o json | python3 -c "
import json,sys
n=[x for x in json.load(sys.stdin) if x['given_name']=='$1']
print(json.dumps(n[0]) if n else '')"; }
wait_for 60 sh -c "docker exec $hs headscale -c /etc/headscale/config.yaml nodes list -o json | grep -q '\"given_name\": *\"k8s-conn\"'" \
  && ok "the connector joined headscale" || die "connector did not join"
n=$(nodejson k8s-conn)
echo "$n" | python3 -c "import json,sys;n=json.load(sys.stdin);t=n.get('tags') or n.get('forced_tags') or [];sys.exit(0 if 'tag:k8s-connector' in t else 1)" \
  && ok "the connector node has tag:k8s-connector" || nok "the connector node is not tagged: $n"
echo "$n" | python3 -c "import json,sys;n=json.load(sys.stdin);k=n.get('pre_auth_key') or {};sys.exit(0 if k.get('ephemeral') else 1)" \
  && ok "the connector joined with an ephemeral key" || nok "the key is not ephemeral"
got=""; i=0
while [ $i -lt 30 ]; do
  got=$(nodejson k8s-conn | python3 -c "import json,sys;n=json.load(sys.stdin);print(','.join(sorted(n.get('approved_routes') or [])))")
  [ "$got" = "10.96.0.0/12" ] && break; i=$((i + 1)); sleep 1
done
[ "$got" = "10.96.0.0/12" ] && ok "policy approves the Service range only (the Pod range stays unapproved)" || nok "approved routes: [$got]"
adv=$(nodejson k8s-conn | python3 -c "import json,sys;n=json.load(sys.stdin);print(','.join(sorted(n.get('available_routes') or [])))")
[ "$adv" = "10.244.0.0/16,10.96.0.0/12" ] && ok "the connector advertises TS_ROUTES" || nok "advertised routes: [$adv]"
st=$(kubectl -n conn get secret tc-state -o jsonpath='{.data}')
echo "$st" | grep -q '_machinekey\|profile' && ok "tailscale state is in the kube Secret tc-state" || nok "state Secret empty: $st"
kubectl -n conn auth can-i list secrets --as=system:serviceaccount:conn:tc >/dev/null 2>&1 \
  && nok "the connector may list Secrets" || ok "the connector cannot list Secrets"
kubectl -n conn auth can-i get secret/tailnet-connector-authkey --as=system:serviceaccount:conn:tc >/dev/null 2>&1 \
  && nok "the connector may read the key Secret" || ok "the connector cannot read other Secrets"

# a tailnet peer reaches the ClusterIP through the connector
docker run -d --name "$cli" --network kind --cap-add NET_ADMIN --device /dev/net/tun \
  -v "$T/ca/ca.pem:/ca.pem:ro" -e SSL_CERT_FILE=/ca.pem -e TS_AUTHKEY="$k_al" -e TS_HOSTNAME=client \
  -e TS_USERSPACE=false -e TS_STATE_DIR=/tmp -e TS_EXTRA_ARGS="--login-server=$url --accept-routes" "$cli_img" >/dev/null || die "client"
if wait_for 60 docker exec "$cli" sh -c "wget -qO- -T 4 http://$svc_ip/ | grep -qx k8s-ok"; then
  ok "a tailnet peer reaches the ClusterIP $svc_ip through the connector"
else nok "the peer cannot reach $svc_ip"; docker exec "$cli" tailscale status 2>&1 | head; fi

# restart: the pod comes back as the same node, from the state Secret
id1=$(nodejson k8s-conn | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
kubectl -n conn rollout restart deploy/tc >/dev/null; kubectl -n conn rollout status deploy/tc --timeout=120s >/dev/null 2>&1
sleep 5
id2=$(nodejson k8s-conn | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])" 2>/dev/null)
[ "$id1" = "$id2" ] && ok "after a restart the connector is the same node ($id1)" || nok "node id changed: $id1 -> $id2"
wait_for 60 docker exec "$cli" sh -c "wget -qO- -T 4 http://$svc_ip/ | grep -qx k8s-ok" \
  && ok "the route works after the restart" || nok "no route after the restart"
echo "# $pass passed, $fails failed"
[ "$fails" = 0 ]

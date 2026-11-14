#!/usr/bin/env bash
# shellcheck disable=SC2015 # ok/nok always succeed
# Static checks of charts/gateway-controller (#338): helm lint, helm template, kubeconform (strict, the Gateway API
# kinds skipped: kubeconform has no schema for them), and the RBAC rules that keep the controller least-privilege.
set -euo pipefail
root=$(cd "$(dirname "$0")/../../.." && pwd)
chart=charts/gateway-controller
cd "$root"
helm_img=${HELM_IMAGE:-alpine/helm:3.17.3}
kc_img=${KUBECONFORM_IMAGE:-ghcr.io/yannh/kubeconform:v0.7.0}
helm() {
  if type -P helm >/dev/null; then command helm "$@"
  else docker run --rm -v "$root:/w" -w /w "$helm_img" "$@"; fi
}
kubeconform() {
  if type -P kubeconform >/dev/null; then command kubeconform "$@"
  else docker run --rm -i "$kc_img" "$@"; fi
}
fails=0
ok()  { echo "ok   $1"; }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

helm lint --strict "$chart" >/dev/null && ok "helm lint" || nok "helm lint"
helm template gc "$chart" --namespace gwc > "$tmp/d.yaml" && ok "helm template" || nok "helm template"
kubeconform -strict -summary -skip GatewayClass -kubernetes-version "${KUBE_VERSION:-1.31.0}" < "$tmp/d.yaml" \
  && ok "kubeconform" || nok "kubeconform"
d=$tmp/d.yaml
[ "$(grep -c '^kind: GatewayClass' "$d")" = 2 ] && ok "two GatewayClasses" || nok "two GatewayClasses"
grep -q 'controllerName: github.com/valentinalexeev/headlamp/gateway-controller' "$d" \
  && ok "controllerName" || nok "controllerName"
grep -qE 'resources: \[.*secrets' "$d" && nok "the ClusterRole names Secrets" || ok "no access to Secrets"
grep -q 'verbs: \[get, list, watch\]' "$d" && ok "read-only on routes and Services" || nok "read verbs"
grep -q '"\*"' "$d" && nok "a wildcard in RBAC" || ok "no wildcard in RBAC"
[ "$(grep -c '^kind: Deployment' "$d")" = 1 ] && ok "one Deployment" || nok "one Deployment"
echo "# $fails failed"
[ "$fails" = 0 ]

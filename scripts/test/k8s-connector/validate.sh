#!/usr/bin/env bash
# shellcheck disable=SC2015 # ok/nok always succeed
# Static checks of charts/tailnet-connector (#103): helm lint, helm template with the default and the example values,
# kubeconform (strict) on every render, and a few structural checks. Uses local helm/kubeconform, or docker images.
set -euo pipefail
root=$(cd "$(dirname "$0")/../../.." && pwd)
chart=charts/tailnet-connector
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
helm lint --strict "$chart" -f "$chart/examples/values-site.yaml" >/dev/null && ok "helm lint (example values)" || nok "helm lint (example values)"
for v in default example; do
  args=(--namespace connector); [ $v = example ] && args+=(-f "$chart/examples/values-site.yaml")
  helm template tc "$chart" "${args[@]}" > "$tmp/$v.yaml" || { nok "helm template $v"; continue; }
  kubeconform -strict -summary -kubernetes-version "${KUBE_VERSION:-1.31.0}" < "$tmp/$v.yaml" && ok "kubeconform $v" || nok "kubeconform $v"
done
d=$tmp/default.yaml; e=$tmp/example.yaml
[ "$(grep -c '^kind: Deployment' "$d")" = 1 ] && ok "default: one Deployment, no serve proxy" || nok "default: one Deployment"
[ "$(grep -c '^kind: Deployment' "$e")" = 3 ] && ok "example: connector and two serve proxies" || nok "example: three Deployments"
grep -q 'replicas: 1$' "$d" && ok "connector replicas 1" || nok "connector replicas 1"
grep -q 'name: TS_USERSPACE, value: "true"' "$d" && ok "userspace mode" || nok "userspace mode"
grep -q 'name: TS_KUBE_SECRET, value: "tc-state"' "$d" && ok "state in a kube Secret" || nok "TS_KUBE_SECRET"
grep -q 'name: TS_ROUTES, value: "10.96.0.0/12,10.244.0.0/16"' "$e" && ok "TS_ROUTES from values" || nok "TS_ROUTES"
grep -q -- '--advertise-tags' "$d" && nok "no --advertise-tags (the key gives the tags)" || ok "no --advertise-tags (the key gives the tags)"
want=$(tr -d '[:space:]' < TAILSCALE_VERSION)
grep -q "^appVersion: \"$want\"$" "$chart/Chart.yaml" && ok "appVersion = TAILSCALE_VERSION" || nok "appVersion"
ver=$(sed -n 's/^version: //p' "$chart/Chart.yaml")
grep -q "image: \"ghcr.io/valentinalexeev/headlamp/tailnet-connector:$ver\"" "$d" && ok "image tag = chart version" || nok "image tag"
# least privilege: the only rule is get/update/patch on the one state Secret
rules=$(awk '/^kind: Role$/{r=1} r&&/^rules:/{p=1;next} p&&/^---/{exit} p' "$d")
if grep -q 'resourceNames: \["tc-state"\]' <<<"$rules" && grep -q 'verbs: \[get, update, patch\]' <<<"$rules" \
  && [ "$(grep -c 'apiGroups' <<<"$rules")" = 1 ]; then ok "Role: one rule, the state Secret only"; else nok "Role least privilege"; fi
grep -q 'kind: ClusterRole' "$d" "$e" && nok "no ClusterRole" || ok "no ClusterRole"
# the schema rejects a bad route and a bad tag
helm template tc "$chart" --set 'connector.routes[0]=nope' >/dev/null 2>&1 && nok "schema rejects a bad route" || ok "schema rejects a bad route"
helm template tc "$chart" --set 'controlURL=headscale' >/dev/null 2>&1 && nok "schema rejects a bad URL" || ok "schema rejects a bad URL"
[ $fails = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

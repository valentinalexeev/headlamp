#!/usr/bin/env bash
set -euo pipefail

: "${HEADSCALE_URL:?HEADSCALE_URL is required, e.g. https://headscale.example.com}"
: "${HEADSCALE_API_KEY:?HEADSCALE_API_KEY is required}"

TAG="${GATEWAY_TAG:-tag:gateway}"
API="${HEADSCALE_URL%/}/api/v1"

auth_header="Authorization: Bearer ${HEADSCALE_API_KEY}"

echo "Looking for nodes with tag: ${TAG}"

nodes_json="$(
  curl -fsS \
    -H "$auth_header" \
    "${API}/node"
)"

node_rows="$(
  echo "$nodes_json" |
  jq -r --arg tag "$TAG" '
    (.nodes // [])[]
    | select((.tags // [] | index($tag)) or (.validTags // [] | index($tag)) or (.forcedTags // [] | index($tag)))
    | [.id, .name, ((.availableRoutes // .subnetRoutes // []) | join(","))]
    | @tsv
  '
)"

if [[ -z "${node_rows:-}" ]]; then
  echo "No nodes found with tag ${TAG}"
  exit 0
fi

while IFS=$'\t' read -r node_id node_name routes_csv; do
  [[ -z "${node_id:-}" ]] && continue

  echo "Processing node ${node_id} (${node_name})"

  routes_json="$(
    echo "$routes_csv" |
    tr ',' '\n' |
    grep -E '^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$' |
    sort -u |
    jq -R . |
    jq -s .
  )"

  route_count="$(echo "$routes_json" | jq 'length')"

  if [[ "$route_count" -eq 0 ]]; then
    echo "No advertised IPv4 routes for node ${node_id}"
    continue
  fi

  echo "Approving ${route_count} routes for node ${node_id}"

  curl -fsS \
    -X POST \
    -H "$auth_header" \
    -H "Content-Type: application/json" \
    --data "$(jq -n --argjson routes "$routes_json" '{routes: $routes}')" \
    "${API}/node/${node_id}/approve_routes" \
    >/dev/null

  echo "Approved routes for node ${node_id}"
done <<< "$node_rows"

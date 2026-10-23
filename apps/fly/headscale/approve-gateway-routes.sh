#!/usr/bin/env bash
set -euo pipefail

: "${HEADSCALE_URL:?HEADSCALE_URL is required, e.g. https://headscale.example.com}"
: "${HEADSCALE_API_KEY:?HEADSCALE_API_KEY is required}"

TAG="${GATEWAY_TAG:-tag:gateway}"
API="${HEADSCALE_URL%/}/api/v1"

auth_header="Authorization: Bearer ${HEADSCALE_API_KEY}"

# Every API call goes through here so a rejected key is reported as such instead of as a bare curl failure
# (an expired key is the usual reason approval silently stops; rotate it with scripts/rotate-headscale-api-key.sh).
api() { # method path [json-body] -> body on stdout; non-2xx -> loud error, exit 2
  local method="$1" path="$2" body="${3:-}" out code
  out="$(mktemp)"
  code="$(curl -sS -o "$out" -w '%{http_code}' -X "$method" -H "$auth_header" \
    ${body:+-H "Content-Type: application/json" --data "$body"} "${API}${path}")" || { rm -f "$out"; echo "ERROR: cannot reach ${API}${path}" >&2; exit 2; }
  if [[ "$code" != 2* ]]; then
    rm -f "$out"
    if [[ "$code" == "401" ]]; then
      echo "ERROR: headscale rejected the API key (HTTP 401): it is expired or invalid; rotate it (scripts/rotate-headscale-api-key.sh)" >&2
    else
      echo "ERROR: ${method} ${path} returned HTTP ${code}" >&2
    fi
    exit 2
  fi
  cat "$out"; rm -f "$out"
}

echo "Looking for nodes with tag: ${TAG}"

nodes_json="$(api GET /node)"

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

  api POST "/node/${node_id}/approve_routes" "$(jq -n --argjson routes "$routes_json" '{routes: $routes}')" >/dev/null

  echo "Approved routes for node ${node_id}"
done <<< "$node_rows"

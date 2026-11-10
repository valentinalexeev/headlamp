#!/bin/sh
# Read-only fleet check (#195). It reads GET /fleet.json from node-control and prints one short table: the
# version and state of node-control, headscale, the funnel ingress, every bridge (agent, xray, sync) and the
# tailscale client of every server (tagged node). It changes nothing. Run it from a tailnet node whose login may
# view the Overview of node-control.
#
#   NODE_CONTROL_URL=http://node-control scripts/fleet-check.sh [--all] [--json]
#
#   --all    list every tailnet node, not only servers
#   --json   print the raw report instead of the table
#
# Exit: 0 when node-control reports no problem, 1 when it reports one (each is printed after the table), 2 when
# the report cannot be read. Env: NODE_CONTROL_URL (required), CURL (default curl).
set -u
base=${NODE_CONTROL_URL:?set NODE_CONTROL_URL to the base URL of node-control on the tailnet}
curl=${CURL:-curl}
all=false
raw=false
while [ $# -gt 0 ]; do
  case "$1" in
    --all) all=true ;;
    --json) raw=true ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
command -v jq >/dev/null 2>&1 || { echo "jq is needed" >&2; exit 2; }

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
code=$($curl -sS -o "$T/fleet.json" -w '%{http_code}' --max-time 60 -H 'Accept: application/json' "$base/fleet.json") || code=000
if [ "$code" != 200 ] || ! jq -e 'type == "object" and has("problems")' "$T/fleet.json" >/dev/null 2>&1; then
  echo "cannot read $base/fleet.json (HTTP $code). node-control before 0.16.0 has no fleet report." >&2
  exit 2
fi
if $raw; then
  cat "$T/fleet.json"
else
  # Every value is reduced to printable characters before it reaches the terminal.
  jq -r --argjson all "$all" '
    def s: tostring | gsub("[^A-Za-z0-9 .:+_/@()-]"; "?") | .[0:60];
    def v: if . == null or . == "" then "-" else s end;
    ["PART", "NAME", "VERSION", "STATE"],
    ["control", "node-control", (.node_control | v), "running"],
    ["control", "headscale", (.headscale | v), (if .headscale_error then "error" else "ok" end)],
    (if .funnel then ["control", "funnel-ingress", (.funnel.version | v),
      (if .funnel.stale then "stale" elif .funnel.errors > 0 then "errors" else "ok" end)] else empty end),
    (.bridges[] | ["bridge", (.name | v), "agent " + (.agent | v) + " xray " + (.xray | v),
      (if .active | not then "not active" elif .online == false then "offline" elif .sync == "pending" then "not in sync"
       else (.sync | v) end)]),
    (.nodes[] | select($all or ((.tags // []) | length > 0)) |
      ["tailscale", (.name | v), (.tailscale | v), (.status | s) + (if .online then "" else ", offline" end)])
    | @tsv' "$T/fleet.json" > "$T/table"
  if command -v column >/dev/null 2>&1; then column -t -s "$(printf '\t')" "$T/table"; else cat "$T/table"; fi
  echo "tailscale pin: $(jq -r '.tailscale_pin // "-"' "$T/fleet.json")"
  jq -r '.notes[]? | "note: " + (tostring | gsub("[^A-Za-z0-9 .:+_/@()-]"; "?"))' "$T/fleet.json"
fi
n=$(jq '.problems | length' "$T/fleet.json")
if [ "$n" -gt 0 ]; then
  $raw || jq -r '.problems[] | "PROBLEM " + (tostring | gsub("[^A-Za-z0-9 .:+_/@()-]"; "?"))' "$T/fleet.json"
  exit 1
fi
$raw || echo "no problems"
exit 0

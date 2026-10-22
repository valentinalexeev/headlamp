#!/usr/bin/env sh
set -eu

# Resolves each host in direct-hosts.txt to its current A records and
# advertises them as /32 routes -- independent of and additive to
# --advertise-exit-node (set once at `tailscale up` in start.sh): setting
# --advertise-routes here does not touch the exit-node flag, so ams stays
# usable both as a full exit node AND as the specific route for these
# hosts regardless of any client's exit-node selection. Same mechanism as
# ansible/roles/route-provisioner's route_hosts_direct on the VM-based
# nodes, simplified: no ASN expansion, no bgpq4/python3 needed
# for a handful of individually-tracked hosts.

HOST_FILE="/app/direct-hosts.txt"
LOG_TAG="ams-direct-routes"

log() {
  echo "[$LOG_TAG] $*"
}

if [ ! -f "$HOST_FILE" ]; then
  log "Host file not found: $HOST_FILE"
  exit 1
fi

tmp_routes="$(mktemp)"
trap 'rm -f "$tmp_routes"' EXIT

while read -r host _comment; do
  case "$host" in
    ""|\#*) continue ;;
  esac

  log "Resolving $host"
  dig +short A "$host" \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' \
    | sed 's#$#/32#' \
    >> "$tmp_routes" || true
done < "$HOST_FILE"

sort -u "$tmp_routes" -o "$tmp_routes"

route_count="$(wc -l < "$tmp_routes" | tr -d ' ')"
if [ "$route_count" -eq 0 ]; then
  log "No routes resolved. Refusing to clear existing advertised routes."
  exit 1
fi

routes_csv="$(paste -sd, "$tmp_routes")"
log "Advertising $route_count route(s): $routes_csv"
/app/tailscale set --advertise-routes="$routes_csv"
log "Done."

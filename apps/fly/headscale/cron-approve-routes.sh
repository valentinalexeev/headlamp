#!/usr/bin/env bash
set -euo pipefail

# A failing run is logged as an ERROR with the number of consecutive failures; the loop itself keeps going so a
# transient outage heals on its own, but an expired API key shows up in `fly logs` on every cycle.
fails=0
while true; do
  if /usr/local/bin/approve-gateway-routes.sh tag:gateway; then
    fails=0
  else
    fails=$((fails + 1))
    echo "ERROR: route approval failed ${fails} time(s) in a row" >&2
  fi
  sleep "${APPROVE_ROUTES_INTERVAL_SECONDS:-300}"
done

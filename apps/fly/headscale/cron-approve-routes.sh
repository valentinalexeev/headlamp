#!/usr/bin/env bash
set -euo pipefail

while true; do
  /usr/local/bin/approve-gateway-routes.sh tag:gateway || true
  sleep "${APPROVE_ROUTES_INTERVAL_SECONDS:-300}"
done

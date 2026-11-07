#!/usr/bin/env bash
# Checks that every tailscale pin in the repo names the version in TAILSCALE_VERSION (#145). Exit 1 and list each pin
# that differs. Usage: scripts/check-tailscale-pins.sh [ROOT]
set -euo pipefail
root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"
want=$(tr -d '[:space:]' < TAILSCALE_VERSION)
[[ $want =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "TAILSCALE_VERSION is not x.y.z: '$want'"; exit 1; }
bad=0
check() { # FILE WHAT GOT
  if [ "$3" != "$want" ]; then echo "MISMATCH $1 ($2): '$3', want '$want'"; bad=1; else echo "ok       $1 ($2): $3"; fi
}
# ARG TSVERSION=<v> in every Dockerfile that has one
while IFS= read -r f; do
  check "$f" "ARG TSVERSION" "$(sed -n 's/^ARG TSVERSION=//p' "$f" | head -1)"
done < <(grep -rl --include='Dockerfile*' '^ARG TSVERSION=' apps)
# the embedded tailscale.com module in every go.mod that requires it
while IFS= read -r f; do
  check "$f" "tailscale.com" "$(sed -n 's/^[[:space:]]*tailscale\.com v//p' "$f" | head -1)"
done < <(grep -rlE --include=go.mod '^[[:space:]]*tailscale\.com v' apps)
# the Ansible role default
check ansible/roles/tailscale/defaults/main.yml tailscale_version \
  "$(sed -n 's/^tailscale_version: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' ansible/roles/tailscale/defaults/main.yml)"
# the Helm chart's appVersion (the tailscale in its image, apps/tailnet-connector)
check charts/tailnet-connector/Chart.yaml appVersion \
  "$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' charts/tailnet-connector/Chart.yaml)"
exit $bad

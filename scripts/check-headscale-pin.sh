#!/usr/bin/env bash
# Checks that every headscale pin in the repo names the version in HEADSCALE_VERSION (#152): the Dockerfile default,
# and the default image of every e2e harness. Exit 1 and list each pin that differs. Usage: check-headscale-pin.sh [ROOT]
set -euo pipefail
root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
cd "$root"
want=$(tr -d '[:space:]' < HEADSCALE_VERSION)
[[ $want =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "HEADSCALE_VERSION is not vX.Y.Z: '$want'"; exit 1; }
bad=0
check() { # FILE WHAT GOT
  if [ "$3" != "$want" ]; then echo "MISMATCH $1 ($2): '$3', want '$want'"; bad=1; else echo "ok       $1 ($2): $3"; fi
}
check apps/headscale/Dockerfile "ARG HEADSCALE_VERSION" "$(sed -n 's/^ARG HEADSCALE_VERSION=//p' apps/headscale/Dockerfile | head -1)"
grep -q '^FROM ghcr.io/juanfont/headscale:\${HEADSCALE_VERSION}$' apps/headscale/Dockerfile \
  || { echo "MISMATCH apps/headscale/Dockerfile (FROM): not the stock ghcr.io/juanfont/headscale image"; bad=1; }
# every harness default: E2E_HEADSCALE_IMAGE:-<image>:<tag>
while IFS= read -r f; do
  while IFS= read -r ref; do
    case $ref in
      ghcr.io/juanfont/headscale:*) check "$f" "E2E_HEADSCALE_IMAGE" "${ref#*:}" ;;
      *) echo "MISMATCH $f (E2E_HEADSCALE_IMAGE): '$ref' is not the stock image"; bad=1 ;;
    esac
  done < <(grep -o 'E2E_HEADSCALE_IMAGE:-[^}]*' "$f" | sed 's/^E2E_HEADSCALE_IMAGE:-//')
done < <(grep -rl --include=e2e-docker.sh 'E2E_HEADSCALE_IMAGE:-' scripts/test)
# other spellings of a headscale image must not appear in the harnesses or the compose example
if grep -rnE --include=e2e-docker.sh --include='*.yaml' --include='*.yml' --include='*.env*' '(jauderho|juanfont)/headscale:v[0-9]' scripts/test examples | grep -v "headscale:$want" | grep -v 'E2E_HEADSCALE_IMAGE:-'; then
  echo "MISMATCH: a headscale image tag above differs from $want"; bad=1
fi
exit $bad

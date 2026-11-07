#!/usr/bin/env bash
# Tests scripts/check-headscale-pin.sh: it passes on this tree, and fails when any one pin differs or a harness, the
# Dockerfile or the compose example names an image other than the stock one.
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
harnesses=$(cd "$root" && grep -rl --include=e2e-docker.sh 'E2E_HEADSCALE_IMAGE:-' scripts/test)
copy() { # a fresh copy of the pinned files
  local d="$tmp/$1"; mkdir -p "$d"
  # shellcheck disable=SC2086
  (cd "$root" && tar -cf - HEADSCALE_VERSION scripts/check-headscale-pin.sh apps/headscale/Dockerfile examples $harnesses) | tar -xf - -C "$d"
  echo "$d"
}
expect() { # NAME pass|fail DIR
  if bash "$3/scripts/check-headscale-pin.sh" "$3" >"$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1 (got $got)"; cat "$tmp/out"; fails=$((fails+1)); fi
}
expect "the repo passes" pass "$root"
d=$(copy v); echo v0.0.1 > "$d/HEADSCALE_VERSION"; expect "a different HEADSCALE_VERSION fails" fail "$d"
d=$(copy bad); echo 0.29.7 > "$d/HEADSCALE_VERSION"; expect "a version without the v prefix fails" fail "$d"
d=$(copy arg); sed -i.bak 's/^ARG HEADSCALE_VERSION=.*/ARG HEADSCALE_VERSION=v0.0.1/' "$d/apps/headscale/Dockerfile"
expect "a different Dockerfile default fails" fail "$d"
d=$(copy from); sed -i.bak 's#^FROM ghcr.io/juanfont/headscale:#FROM jauderho/headscale:#' "$d/apps/headscale/Dockerfile"
expect "a Dockerfile on another image fails" fail "$d"
i=0
for f in $harnesses; do
  i=$((i+1))
  d=$(copy "h$i"); sed -i.bak 's#E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:[^}]*#E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.0.1#' "$d/$f"
  expect "a different tag in $f fails" fail "$d"
  d=$(copy "o$i"); sed -i.bak 's#E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:#E2E_HEADSCALE_IMAGE:-jauderho/headscale:#' "$d/$f"
  expect "another image in $f fails" fail "$d"
done
d=$(copy x); printf '# image: jauderho/headscale:v0.0.1\n' >> "$d/examples/compose/control/compose.yaml"
expect "a stray headscale tag in the examples fails" fail "$d"
[ "$fails" -eq 0 ] && echo "all headscale-pin tests passed" || { echo "$fails failed"; exit 1; }

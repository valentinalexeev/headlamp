#!/usr/bin/env bash
# Tests scripts/check-tailscale-pins.sh: it passes on this tree, and fails when any one pin differs.
set -u
root=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
copy() { # a fresh copy of the pinned files
  local d="$tmp/$1"; mkdir -p "$d"
  # shellcheck disable=SC2046 # file lists from grep are split on purpose (paths have no spaces)
  (cd "$root" && tar -cf - TAILSCALE_VERSION scripts/check-tailscale-pins.sh ansible/roles/tailscale/defaults/main.yml \
    charts/tailnet-connector/Chart.yaml $(grep -rl --include='Dockerfile*' '^ARG TSVERSION=' apps) $(grep -rlE --include=go.mod '^[[:space:]]*tailscale\.com v' apps)) | tar -xf - -C "$d"
  echo "$d"
}
expect() { # NAME pass|fail DIR
  if bash "$3/scripts/check-tailscale-pins.sh" "$3" >"$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1 (got $got)"; cat "$tmp/out"; fails=$((fails+1)); fi
}
expect "the repo passes" pass "$root"
i=0
for f in TAILSCALE_VERSION ansible/roles/tailscale/defaults/main.yml charts/tailnet-connector/Chart.yaml \
  $(cd "$root" && grep -rl --include='Dockerfile*' '^ARG TSVERSION=' apps) \
  $(cd "$root" && grep -rlE --include=go.mod '^[[:space:]]*tailscale\.com v' apps); do
  i=$((i+1)); d=$(copy "c$i")
  case "$f" in
  TAILSCALE_VERSION) echo 0.0.1 > "$d/$f" ;;
  *defaults/main.yml) sed -i.bak 's/^tailscale_version: .*/tailscale_version: "0.0.1"/' "$d/$f" ;;
  *Chart.yaml) sed -i.bak 's/^appVersion: .*/appVersion: "0.0.1"/' "$d/$f" ;;
  *go.mod) sed -i.bak 's/^\([[:space:]]*tailscale\.com\) v.*/\1 v0.0.1/' "$d/$f" ;;
  *) sed -i.bak 's/^ARG TSVERSION=.*/ARG TSVERSION=0.0.1/' "$d/$f" ;;
  esac
  expect "a different pin in $f fails" fail "$d"
done
d=$(copy bad); echo "1.2" > "$d/TAILSCALE_VERSION"
expect "a malformed TAILSCALE_VERSION fails" fail "$d"
[ $fails = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

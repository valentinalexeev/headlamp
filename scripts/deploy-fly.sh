#!/bin/sh
# Overlay deploy of one Fly app from a site repo that vendors headlamp (#194). Lay the site's files over the vendored
# app directory, deploy, and put the submodule back as it was. Run it from anywhere in the site repo.
#
#   deploy-fly.sh <app> [flyctl deploy args...]
#   deploy-fly.sh --dry-run <app> [flyctl deploy args...]   print the flyctl command, deploy nothing
#
#   <app>  a directory under vendor/headlamp/apps (headscale, exit-node, node-control, funnel-ingress, ...)
#
# It refuses when the submodule has changes, when its checked-out commit is not the commit the site repo pins (so an
# uncommitted pin bump is never deployed or reset), or when there is no overlay. It passes these build args:
#   VERSION=<vendor/headlamp/VERSION>        to the apps that declare `ARG VERSION` (node-control shows it)
#   TSVERSION=<site TAILSCALE_VERSION>       to the apps that declare `ARG TSVERSION` (the fleet tailscale pin; the
#                                            submodule's pin when the site has no TAILSCALE_VERSION file)
# A build arg the caller gives with --build-arg wins.
#
# Environment:
#   HEADLAMP_DIR   the submodule path, relative to the site root (default vendor/headlamp)
#   OVERLAY_DIR    the overlay root, relative to the site root (default fly); the files of <app> are in OVERLAY_DIR/<app>/
#   FLYCTL         the flyctl command (default flyctl; tests set a fake)
set -eu

dry=0
[ "${1:-}" = --dry-run ] && { dry=1; shift; }
app="${1:?usage: deploy-fly.sh [--dry-run] <app> [flyctl deploy args...]}"; shift

die() { echo "deploy-fly: $*" >&2; exit 1; }
root="$(git rev-parse --show-superproject-working-tree 2>/dev/null)"
[ -n "$root" ] || root="$(git rev-parse --show-toplevel 2>/dev/null)" || die "not inside a git repo"
cd "$root"
hl="${HEADLAMP_DIR:-vendor/headlamp}"
ov="${OVERLAY_DIR:-fly}"
[ -d "$hl/apps" ] || die "no $hl/apps in $root (set HEADLAMP_DIR)"
case "$app" in */* | .* | "") die "bad app name: $app" ;; esac
dst="$hl/apps/$app"; src="$ov/$app"
[ -d "$dst" ] || die "no app $app in $hl/apps"
[ -d "$src" ] || die "no overlay $src/ for $app"

# The submodule must be clean and at the commit the site pins, else a pin bump that is not committed yet would go
# live, or the cleanup below would throw it away.
[ -z "$(git -C "$hl" status --porcelain)" ] || die "$hl has changes; commit or remove them first"
pinned="$(git ls-tree HEAD "$hl" | awk '{print $3}')"
staged="$(git ls-files -s "$hl" | awk '{print $2}')"
head="$(git -C "$hl" rev-parse HEAD)"
[ -n "$staged" ] || die "$hl is not a submodule of this repo"
[ "$staged" = "$head" ] || die "$hl is at $head, but the index pins $staged (run 'git add $hl' or 'git submodule update')"
[ -z "$pinned" ] || [ "$pinned" = "$staged" ] || echo "deploy-fly: note: the pin of $hl is staged but not committed ($staged)" >&2

# Build args from the pins, for the args that the Dockerfile declares. A --build-arg from the caller wins.
has_arg() { grep -qE "^ARG $1(=|\$| )" "$dst/Dockerfile" 2>/dev/null; }
given() { # given NAME ARGS...: ARGS contain "--build-arg NAME=..." or "--build-arg=NAME=..."
  n="$1"; shift; prev=""
  for a in "$@"; do
    case "$prev:$a" in --build-arg:"$n"=* | *:--build-arg="$n"=*) return 0 ;; esac
    prev="$a"
  done
  return 1
}
extra=""
if has_arg VERSION && ! given VERSION "$@"; then
  extra="$extra --build-arg VERSION=$(tr -d '[:space:]' < "$hl/VERSION")"
fi
if has_arg TSVERSION && ! given TSVERSION "$@"; then
  tsf=TAILSCALE_VERSION; [ -f "$tsf" ] || tsf="$hl/TAILSCALE_VERSION"
  extra="$extra --build-arg TSVERSION=$(tr -d '[:space:]' < "$tsf")"
fi

flyctl="${FLYCTL:-flyctl}"
if [ "$dry" = 1 ]; then
  # shellcheck disable=SC2086 # extra is a list of words built above
  echo "would run in $dst (with $src/ laid over it): $flyctl deploy$extra $*"
  exit 0
fi

trap 'git -C "$hl" checkout -q -- . && git -C "$hl" clean -fdq' EXIT INT TERM
cp -R "$src"/. "$dst"/
# shellcheck disable=SC2086
( cd "$dst" && $flyctl deploy $extra "$@" )

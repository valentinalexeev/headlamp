#!/bin/sh
# Overlay deploy: build a Fly app from the vendored core repo using YOUR config files.
# The Dockerfiles COPY site files (headscale.yaml, acl.json, direct-hosts.txt, app/nodes.json) from the
# app directory, so your copies are laid over the vendored directory for the build and then
# the submodule is restored to its pristine state.
#
#   fly/deploy.sh <headscale|exit-ams|node-control> [flyctl deploy args...]
#   e.g. fly/deploy.sh exit-ams --build-arg TSVERSION=1.2.3
#
# Your files live in fly/<app>/ (same relative layout as vendor/headlamp/apps/fly/<app>/).
set -eu
app="${1:?usage: fly/deploy.sh <app-dir> [flyctl deploy args...]}"; shift
src="fly/$app"; dst="vendor/headlamp/apps/fly/$app"
[ -d "$src" ] || { echo "no $src/ (your overlay files for $app)" >&2; exit 1; }
[ -z "$(git -C vendor/headlamp status --porcelain)" ] || { echo "vendor/headlamp is not clean; refusing" >&2; exit 1; }
trap 'git -C vendor/headlamp checkout -- . && git -C vendor/headlamp clean -fdq' EXIT
cp -R "$src"/. "$dst"/
( cd "$dst" && flyctl deploy "$@" )

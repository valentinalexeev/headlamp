#!/bin/sh
# Deprecated (#194): use `make deploy-<app>`, or vendor/headlamp/scripts/deploy-fly.sh directly. This wrapper keeps
# old site scripts working. It runs the headlamp script, which you do not copy.
exec "$(git rev-parse --show-toplevel)/vendor/headlamp/scripts/deploy-fly.sh" "$@"

#!/bin/sh
# Entrypoint of the headlamp tailguard image: runs the upstream entrypoint after one fix of the environment.
#
# The upstream entrypoint takes an unset TG_WEBUI_PORT as "status page off", but exits on an empty one. The compose
# file sets TG_WEBUI_PORT from TG_STATUS_PORT, and an empty TG_STATUS_PORT is the documented way to keep the status
# page off (#390). Compose cannot leave a variable out, so this unsets an empty TG_WEBUI_PORT.
set -eu
if [ -z "${TG_WEBUI_PORT:-}" ]; then
  unset TG_WEBUI_PORT
fi
exec /tailguard/entrypoint.sh "$@"

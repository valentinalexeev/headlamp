#!/bin/sh
# Runs tailscaled in userspace mode with its state in a Kubernetes Secret, then "tailscale up". Settings (the chart
# sets them): TS_AUTHKEY, TS_KUBE_SECRET (state Secret, in the pod's namespace), TS_HOSTNAME, TS_ROUTES (comma list),
# TS_EXTRA_ARGS (more "tailscale up" flags), TS_SOCKET (default /tmp/tailscaled.sock).
set -eu
sock=${TS_SOCKET:-/tmp/tailscaled.sock}
state=${TS_STATE:-kube:${TS_KUBE_SECRET:?TS_KUBE_SECRET is required}}
tailscaled --tun=userspace-networking --socket="$sock" --state="$state" --statedir=/tmp/tailscale &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null; wait "$pid"; exit 0' TERM INT
i=0; until tailscale --socket="$sock" status --json >/dev/null 2>&1 || [ -S "$sock" ]; do
  i=$((i + 1)); [ $i -gt 60 ] && { echo "tailscaled did not start" >&2; exit 1; }; sleep 1
done
# shellcheck disable=SC2086 # TS_EXTRA_ARGS is a list of flags
tailscale --socket="$sock" up --authkey="${TS_AUTHKEY:?TS_AUTHKEY is required}" --hostname="${TS_HOSTNAME:-$(hostname)}" \
  --advertise-routes="${TS_ROUTES:-}" --accept-dns="${TS_ACCEPT_DNS:-false}" --reset ${TS_EXTRA_ARGS:-}
echo "tailscale is up: $(tailscale --socket="$sock" ip -4 2>/dev/null)"
wait "$pid"

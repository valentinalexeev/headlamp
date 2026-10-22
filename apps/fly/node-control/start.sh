#!/bin/sh
set -e

/app/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/var/run/tailscale/tailscaled.sock &

# NOTE: no --advertise-tags here. This headscale server rejects any client-
# requested tags on preauth-key registrations outright (regardless of ACL
# tagOwners) -- see hscontrol/state/state.go's createAndSaveNewNode: tags for
# a preauth-key-registered node must come from the key itself, set at
# creation time via `headscale preauthkeys create --tags tag:node-control`
# (see apps/fly/node-control/secrets.sops.yaml). Passing --advertise-tags
# here fails registration with "requested tags [...] are invalid or not
# permitted", even though the key already carries the tag.
/app/tailscale up \
  --login-server="${HEADSCALE_LOGIN_SERVER}" \
  --authkey="${TAILSCALE_AUTH_KEY}" \
  --hostname="${TAILNET_HOSTNAME:-node-control}" \
  --accept-dns=false

# tailscale up returns once auth succeeds; the interface/address can take a
# moment longer to actually be assigned. Wait for it rather than racing.
TS_IP=""
for i in $(seq 1 30); do
  TS_IP="$(/app/tailscale ip -4 2>/dev/null || true)"
  [ -n "$TS_IP" ] && break
  sleep 1
done

if [ -z "$TS_IP" ]; then
  echo "tailscale did not get an IPv4 address in time" >&2
  exit 1
fi

# Bind ONLY to the tailscale IP -- never 0.0.0.0. This is the app's actual
# network isolation boundary (see the design doc's Architecture section):
# no public listener exists at all, regardless of Fly's own proxy/firewall
# configuration, since fly.toml also declares no [[services]].
echo "Binding to tailscale IP: $TS_IP"

# Two listeners, both on the tailscale IP only:
#   :80   admin app (nodes UI/API and, when ENROLL_DB is set, the /enroll admin pages)
#   :8081 public enrolment (only started when ENROLL_DB is set). It serves end users
#         through the ingress proxy, so the tailnet ACL must allow :8081 from the ingress
#         proxy only; never expose it any wider.
if [ -n "${ENROLL_DB:-}" ]; then
  echo "ENROLL_DB set: starting public enrolment listener on $TS_IP:8081"
  # The enrolment listener is a background process, so on its own its death would go unnoticed
  # (Fly would see a healthy admin app on :80). Run it under a small supervisor subshell that
  # records its exit and, once startup is over, terminates this process (which becomes the
  # main uvicorn via exec below) so that Fly restarts the whole machine.
  ENROLL_STATE="$(mktemp -d)"
  MAIN_PID=$$
  (
    # --no-access-log: request paths on this listener carry tokens/secrets, never log them.
    uvicorn app.enroll.public:create_app_from_env --factory --host "$TS_IP" --port 8081 --no-access-log &
    wait $! || true   # set -e: a non-zero exit must still reach the lines below
    touch "$ENROLL_STATE/dead"
    if [ -e "$ENROLL_STATE/started" ]; then
      echo "enrolment listener exited; terminating so the machine restarts" >&2
      kill -TERM "$MAIN_PID" 2>/dev/null || true
    fi
  ) &
  # Fail loudly on a misconfigured deploy (missing ENROLL_CONFIG / ENROLL_SECRET_KEY, port in use).
  sleep 3
  if [ -e "$ENROLL_STATE/dead" ]; then
    echo "enrolment listener failed to start (check ENROLL_CONFIG, ENROLL_SECRET_KEY, port 8081); see log above" >&2
    exit 1
  fi
  touch "$ENROLL_STATE/started"
fi

# Keep the main listener last and in the foreground (exec) so it stays the container's main process.
exec uvicorn app.main:app --host "$TS_IP" --port 80

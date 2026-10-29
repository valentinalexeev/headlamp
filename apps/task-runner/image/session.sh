#!/bin/sh
# Browser session entrypoint: TigerVNC's Xvnc (X server and VNC server in one,
# loopback only) + openbox + headed Chrome, exposed through noVNC. Xvnc accepts
# client-driven desktop resizing, so a noVNC client using resize=remote gets a
# desktop with exactly the size of its browser tab (1:1 pixels). No VNC
# password: the network ACL gates SESSION_NOVNC_PORT and websockify only
# connects a WebSocket that carries
# ?token=$SESSION_TOKEN. The token lives only in the environment and in a 0600
# file; it is never put on a command line or printed. Everything is written
# under /tmp.
#
# Env: SESSION_IDLE_SECONDS  exit 0 after this long without a VNC client
#                            (counted from start until the first client, and
#                            after each disconnect; a connected client is never
#                            cut off)
#      SESSION_NOVNC_PORT    port noVNC listens on (all interfaces)
#      SESSION_TOKEN         secret a WebSocket client must present as ?token=
set -u

IDLE="${SESSION_IDLE_SECONDS:?SESSION_IDLE_SECONDS is required}"
PORT="${SESSION_NOVNC_PORT:?SESSION_NOVNC_PORT is required}"
TOKEN="${SESSION_TOKEN:?SESSION_TOKEN is required}"
NOVNC_ROOT="${NOVNC_ROOT:-/usr/share/novnc}"
DISPLAY=:99
export DISPLAY

RUN=/tmp/session
rm -rf "$RUN"
mkdir -p "$RUN/profile" "$RUN/xdg"
# websockify token file: one "<token>: host:port" line, readable only by us.
(umask 077 && printf '%s: 127.0.0.1:5900\n' "$TOKEN" > "$RUN/tokens") || exit 1
unset SESSION_TOKEN TOKEN
export XDG_CONFIG_HOME="$RUN/xdg" XDG_CACHE_HOME="$RUN/xdg"

PIDS=""
cleanup() {
  # shellcheck disable=SC2086
  [ -n "$PIDS" ] && kill $PIDS 2>/dev/null
  wait 2>/dev/null
}
trap 'cleanup; exit 143' TERM INT

start() { # start <cmd...>: background a child and remember its pid
  "$@" &
  PIDS="$PIDS $!"
}

# Xvnc is the X server and the VNC server: loopback only, no VNC password, and
# it accepts desktop size changes requested by the client (noVNC resize=remote).
start Xvnc "$DISPLAY" -geometry 1280x800 -depth 24 -rfbport 5900 -localhost yes \
  -SecurityTypes None -AcceptSetDesktopSize=1 -AlwaysShared=1 -nolisten tcp \
  -desktop session >/dev/null 2>&1
# Wait for the X socket.
i=0
while [ ! -S /tmp/.X11-unix/X99 ] && [ "$i" -lt 100 ]; do
  sleep 0.1
  i=$((i + 1))
done

start openbox

# Real Chrome when the image has it, Playwright's Chromium otherwise.
CHROME=/opt/google/chrome/chrome
[ -x "$CHROME" ] || CHROME=$(node -e "console.log(require('playwright').chromium.executablePath())")
start "$CHROME" --no-sandbox --disable-dev-shm-usage --no-first-run \
  --no-default-browser-check --password-store=basic --user-data-dir="$RUN/profile" \
  --start-maximized about:blank

start websockify --web "$NOVNC_ROOT" --token-plugin=TokenFile --token-source="$RUN/tokens" "$PORT" >/dev/null 2>&1

# Connected VNC clients = established connections to Xvnc's port (5900 = 0x170C),
# which websockify holds one of per noVNC client; read from /proc (no ss in the image).
clients() {
  cat /proc/net/tcp /proc/net/tcp6 2>/dev/null |
    awk '$2 ~ /:170C$/ && $4 == "01" { n++ } END { print n + 0 }'
}

idle_since=$(date +%s)
while :; do
  for p in $PIDS; do
    if ! kill -0 "$p" 2>/dev/null; then
      echo "session: child $p exited" >&2
      cleanup
      exit 1
    fi
  done
  now=$(date +%s)
  if [ "$(clients)" -gt 0 ]; then
    idle_since=$now
  elif [ $((now - idle_since)) -ge "$IDLE" ]; then
    echo "session: idle for ${IDLE}s, exiting" >&2
    cleanup
    exit 0
  fi
  sleep 1
done

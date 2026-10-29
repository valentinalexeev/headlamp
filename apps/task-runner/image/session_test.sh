#!/bin/sh
# Static checks for session.sh (the image cannot be built everywhere).
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
f="$dir/session.sh"
fail() { echo "FAIL: $1" >&2; exit 1; }
[ -f "$f" ] || fail "session.sh missing"
sh -n "$f" || fail "syntax error in session.sh"
for k in -localhost SESSION_IDLE_SECONDS SESSION_NOVNC_PORT -nopw -forever -shared Xvfb websockify; do
  grep -q -- "$k" "$f" || fail "missing $k"
done
if grep -Eq -- '-passwd|-rfbauth' "$f"; then fail "VNC password option used"; fi
echo "PASS"

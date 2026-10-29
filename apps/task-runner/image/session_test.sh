#!/bin/sh
# Static checks for session.sh (the image cannot be built everywhere).
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
f="$dir/session.sh"
fail() { echo "FAIL: $1" >&2; exit 1; }
[ -f "$f" ] || fail "session.sh missing"
sh -n "$f" || fail "syntax error in session.sh"
for k in -localhost SESSION_IDLE_SECONDS SESSION_NOVNC_PORT -nopw -forever -shared Xvfb websockify SESSION_TOKEN --token-plugin=TokenFile --token-source=; do
  grep -q -- "$k" "$f" || fail "missing $k"
done
if grep -Eq -- '-passwd|-rfbauth' "$f"; then fail "VNC password option used"; fi
if grep -Eq -- 'websockify .*127\.0\.0\.1:5900' "$f"; then fail "websockify still has a fixed target"; fi
grep -q -- 'umask 077\|chmod 600' "$f" || fail "token file is not created 0600"
if grep -Eq -- '(echo|printf).*SESSION_TOKEN.*(>&2|logger)|set -x' "$f"; then fail "token may be logged"; fi
echo "PASS"

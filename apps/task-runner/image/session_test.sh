#!/bin/sh
# Static checks for session.sh (the image cannot be built everywhere).
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
f="$dir/session.sh"
fail() { echo "FAIL: $1" >&2; exit 1; }
[ -f "$f" ] || fail "session.sh missing"
sh -n "$f" || fail "syntax error in session.sh"
for k in "-localhost yes" SESSION_IDLE_SECONDS SESSION_NOVNC_PORT "-SecurityTypes None" -AcceptSetDesktopSize=1 Xvnc /proc/net/tcp websockify SESSION_TOKEN --token-plugin=TokenFile --token-source=; do
  grep -q -- "$k" "$f" || fail "missing $k"
done
if grep -Eq -- '-passwd|-rfbauth|PlainUsers|SecurityTypes (VncAuth|Plain)' "$f"; then fail "VNC password option used"; fi
if grep -Eq -- 'x11vnc|Xvfb|RFB_CLIENT_ID|--window-size' "$f"; then fail "leftover of the Xvfb/x11vnc setup or a fixed window size"; fi
if grep -Eq -- 'websockify .*127\.0\.0\.1:5900' "$f"; then fail "websockify still has a fixed target"; fi
grep -Eq -- 'umask 077|chmod 600' "$f" || fail "token file is not created 0600"
if grep -Eq -- '(echo|printf).*SESSION_TOKEN.*(>&2|logger)|set -x' "$f"; then fail "token may be logged"; fi
grep -Eq -- '--token-source=.*>/dev/null 2>&1' "$f" || fail "websockify output is not discarded"
grep -q -- '/opt/google/chrome/chrome' "$f" || fail "session.sh does not prefer Google Chrome"
grep -q -- 'playwright' "$f" || fail "session.sh has no Chromium fallback"
cf="$dir/Containerfile"
grep -q -- 'tigervnc-standalone-server' "$cf" || fail "Containerfile does not install Xvnc"
grep -q -- 'playwright install chrome' "$cf" || fail "Containerfile does not install Chrome"
grep -q -- 'CommandLineFlagSecurityWarningsEnabled' "$cf" || fail "Containerfile has no flag-warning policy"
grep -q -- '^respawn()' "$f" || fail "no respawn supervision for the window manager and the browser"
grep -q -- 'respawn openbox' "$f" || fail "openbox is not respawned"
grep -q -- 'respawn "\$CHROME"' "$f" || fail "the browser is not respawned"
grep -q -- 'openbox/menu.xml' "$f" || fail "no custom openbox menu"
if grep -Eq -- '<action name="Exit"|start openbox|start "\$CHROME"' "$f"; then fail "openbox Exit menu entry or a non-respawned child"; fi
echo "PASS"

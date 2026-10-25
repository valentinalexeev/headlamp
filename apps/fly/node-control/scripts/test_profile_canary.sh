#!/bin/sh
# Tests for profile-canary.sh against two local fake servers. No network beyond 127.0.0.1.
# SCRIPT=<path> runs the tests against another copy of the script (used for mutation checks).
set -u
dir=$(cd "$(dirname "$0")" && pwd)
script=${SCRIPT:-$dir/profile-canary.sh}
t=$(mktemp -d) || exit 2
pids=""
stopall() { for p in $pids; do { kill "$p" && wait "$p"; } 2>/dev/null; done; pids=""; }
# shellcheck disable=SC2329
cleanup() { stopall; rm -rf "$t"; }
trap cleanup EXIT
fail=0
[ -f "$script" ] || { echo "FAIL script missing: $script"; exit 1; }
check() { # check <name> <test args...>
  name=$1; shift
  if [ "$@" ]; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}

SECRET=12345.AbCdEf_gh-IJ
BODY_MARKER=PROFILE-BODY-MARKER-7f3a

# fake <name> <body> [status]: serves <body> at /sub/<SECRET> on a free loopback port; port in $t/<name>.port
fake() {
  rm -f "$t/$1.port" "$t/$1.port.hits" # never read a port left by an earlier server of the same name
  printf '%s' "$2" >"$t/$1.body"
  python3 - "$t/$1.body" "$t/$1.port" "$SECRET" "${3:-200}" <<'PY' &
import sys, http.server
body_path, port_path, secret, status = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        open(port_path + ".hits", "a").write("x")
        ok = self.path == "/sub/" + secret and self.headers.get("User-Agent") == "profile-canary"
        data = open(body_path, "rb").read() if ok else b"no"
        self.send_response(status if ok else 404)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(port_path, "w").write(str(s.server_address[1]))
s.serve_forever()
PY
  pids="$pids $!"
  i=0
  while [ ! -s "$t/$1.port" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
}

run() { # run <direct-port> <link-port> [secret]
  CANARY_TEST=1 CANARY_DIRECT_PORT=$1 CANARY_LINK_PORT=$2 CANARY_LINK_SCHEME=http \
    sh "$script" http://127.0.0.1 127.0.0.1 "${3:-$SECRET}" >"$t/out" 2>"$t/err"
  rc=$?
  cat "$t/out" "$t/err" >"$t/all"
}

fake a "$BODY_MARKER profile one"
fake b "$BODY_MARKER profile one"
fake c "$BODY_MARKER profile TWO"
pa=$(cat "$t/a.port"); pb=$(cat "$t/b.port"); pc=$(cat "$t/c.port")

# same body: OK, exit 0
run "$pa" "$pb"
check "same body exits 0" "$rc" -eq 0
check "same body prints OK" "$(grep -c '^OK' "$t/out")" -ge 1

# different body: MISMATCH, exit 1 (status codes are both 200, so only the bodies differ)
run "$pa" "$pc"
check "different body exits 1" "$rc" -eq 1
check "different body prints MISMATCH" "$(grep -c MISMATCH "$t/out")" -ge 1

# the two short hashes are printed and differ on mismatch
h1=$(grep -o 'direct=[0-9a-f]*' "$t/out" | cut -d= -f2)
h2=$(grep -o 'link=[0-9a-f]*' "$t/out" | cut -d= -f2)
check "hashes printed" -n "$h1" -a -n "$h2"
check "hashes are short" "${#h1}" -le 16
check "hashes differ on mismatch" "$h1" != "$h2"

# never the secret or any body text
check "secret never echoed" "$(grep -c "$SECRET" "$t/all")" -eq 0
check "secret tail never echoed" "$(grep -c 'AbCdEf' "$t/all")" -eq 0
check "body never echoed" "$(grep -c "$BODY_MARKER" "$t/all")" -eq 0
check "no body words on mismatch" "$(grep -c 'profile' "$t/all")" -eq 0

# one side down: exit 2, still no secret
stopall
run "$pa" "$pb"
check "both down exits 2" "$rc" -eq 2
check "fetch error does not echo the secret" "$(grep -c "$SECRET" "$t/all")" -eq 0
fake a "$BODY_MARKER profile one"; pa=$(cat "$t/a.port")
run "$pa" 1
check "link side down exits 2" "$rc" -eq 2
check "link side down prints no OK" "$(grep -c '^OK' "$t/out")" -eq 0

# a failing status (the server answers 500) is an error, not a comparison
fake e "$BODY_MARKER profile one" 500; pe=$(cat "$t/e.port")
fake f "$BODY_MARKER profile one" 500; pf=$(cat "$t/f.port")
run "$pe" "$pf"
check "HTTP 500 on both exits 2" "$rc" -eq 2

# a redirect is not followed, and not taken for a profile: one side or both sides
cat >"$t/redir.py" <<'PY'
import sys, http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(302); self.send_header("Location", "http://127.0.0.1:1/x"); self.send_header("Content-Length", "0"); self.end_headers()
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
for n in r1 r2; do
  rm -f "$t/$n.port"
  python3 "$t/redir.py" "$t/$n.port" & pids="$pids $!"
  i=0; while [ ! -s "$t/$n.port" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
done
pr1=$(cat "$t/r1.port"); pr2=$(cat "$t/r2.port")
run "$pa" "$pr1"
check "redirect on the link side exits 2" "$rc" -eq 2
check "redirect on the link side prints no OK" "$(grep -c '^OK' "$t/out")" -eq 0
run "$pr1" "$pa"
check "redirect on the direct side exits 2" "$rc" -eq 2
run "$pr1" "$pr2"
check "redirects on both sides exit 2 (two empty bodies are not a match)" "$rc" -eq 2
check "redirects on both sides print no OK" "$(grep -c '^OK' "$t/out")" -eq 0

# an empty 200 body is not a profile, even when both sides agree
fake g ""; pg=$(cat "$t/g.port")
fake h ""; ph=$(cat "$t/h.port")
run "$pg" "$ph"
check "two empty bodies exit 2" "$rc" -eq 2
check "two empty bodies print no OK" "$(grep -c '^OK' "$t/out")" -eq 0
run "$pa" "$ph"
check "one empty body exits 2" "$rc" -eq 2

# the test hooks are ignored unless CANARY_TEST=1: the fake must see no request at all
fake m "$BODY_MARKER profile one"; pm=$(cat "$t/m.port")
CANARY_DIRECT_PORT="$pm" CANARY_LINK_PORT="$pm" CANARY_LINK_SCHEME=http \
  sh "$script" http://127.0.0.1 127.0.0.1 "$SECRET" >"$t/out" 2>"$t/err"
check "hooks without CANARY_TEST print no OK" "$(grep -c '^OK' "$t/out")" -eq 0
check "hooks without CANARY_TEST never reach the fake" "$(cat "$t/m.port.hits" 2>/dev/null | wc -c | tr -d ' ')" -eq 0

# the secret can come from the environment or stdin instead of the argument list
fake k "$BODY_MARKER profile one"; pk=$(cat "$t/k.port")
CANARY_TEST=1 CANARY_DIRECT_PORT=$pa CANARY_LINK_PORT=$pk CANARY_LINK_SCHEME=http CANARY_SECRET=$SECRET \
  sh "$script" http://127.0.0.1 127.0.0.1 >"$t/out" 2>"$t/err"
check "secret from CANARY_SECRET" "$?" -eq 0
printf '%s\n' "$SECRET" | CANARY_TEST=1 CANARY_DIRECT_PORT=$pa CANARY_LINK_PORT=$pk CANARY_LINK_SCHEME=http \
  sh "$script" http://127.0.0.1 127.0.0.1 - >"$t/out" 2>"$t/err"
check "secret from stdin" "$?" -eq 0
printf '%s' "$SECRET" | CANARY_TEST=1 CANARY_DIRECT_PORT=$pa CANARY_LINK_PORT=$pk CANARY_LINK_SCHEME=http \
  sh "$script" http://127.0.0.1 127.0.0.1 - >"$t/out" 2>"$t/err"
check "secret on stdin without a trailing newline" "$?" -eq 0
printf 'x\n' | CANARY_TEST=1 CANARY_DIRECT_PORT=$pa CANARY_LINK_PORT=$pk CANARY_LINK_SCHEME=http \
  sh "$script" http://127.0.0.1 127.0.0.1 - >"$t/out" 2>"$t/err"
check "bad secret on stdin exits 2" "$?" -eq 2

# control characters (a newline hides a second line from line-based checks) are refused
nl='
'
CANARY_DIRECT_PORT=1 sh "$script" "http://127.0.0.1${nl}x" 127.0.0.1 "$SECRET" >"$t/out" 2>"$t/err"; check "newline in the base url exits 2" "$?" -eq 2
CANARY_DIRECT_PORT=1 sh "$script" http://127.0.0.1 "127.0.0.1${nl}evil.example" "$SECRET" >"$t/out" 2>"$t/err"; check "newline in the host exits 2" "$?" -eq 2
CANARY_DIRECT_PORT=1 sh "$script" http://127.0.0.1 127.0.0.1 "$SECRET${nl}x" >"$t/out" 2>"$t/err"; check "newline in the secret exits 2" "$?" -eq 2
CANARY_DIRECT_PORT=1 sh "$script" http://127.0.0.1 127.0.0.1 "$SECRET$(printf '\001')" >"$t/out" 2>"$t/err"; check "control character in the secret exits 2" "$?" -eq 2

# argument checks: exit 2, and nothing is fetched
for args in "" "http://127.0.0.1 127.0.0.1" "-x 127.0.0.1 $SECRET" "http://127.0.0.1 -bad.example $SECRET" \
  "http://127.0.0.1 bad_host $SECRET" "http://127.0.0.1 127.0.0.1 nosecret" "http://127.0.0.1 127.0.0.1 12.a/b" \
  "ftp://127.0.0.1 127.0.0.1 $SECRET" "http://127.0.0.1/path 127.0.0.1 $SECRET" "http://127.0.0.1 127.0.0.1 $SECRET extra"; do
  # shellcheck disable=SC2086
  CANARY_DIRECT_PORT=1 sh "$script" $args >"$t/out" 2>"$t/err"; rc=$?
  check "usage error exits 2: ${args%% *}" "$rc" -eq 2
done
# a secret passed as an argument that fails validation is not echoed back
CANARY_DIRECT_PORT=1 sh "$script" http://127.0.0.1 127.0.0.1 'secret-with-no-dot' >"$t/out" 2>"$t/err"
check "rejected secret is not echoed" "$(cat "$t/out" "$t/err" | grep -c 'secret-with-no-dot')" -eq 0

exit $fail

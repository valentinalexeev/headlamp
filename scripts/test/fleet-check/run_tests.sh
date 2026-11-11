#!/bin/sh
# Tests for scripts/fleet-check.sh (#195) with a fake curl. No network.
#   sh scripts/test/fleet-check/run_tests.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../fleet-check.sh"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
fails=0
# The fake curl writes $T/body to the -o file and prints $T/code.
cat > "$T/curl" <<'S'
#!/bin/sh
out=""
while [ $# -gt 0 ]; do [ "$1" = -o ] && { out=$2; shift; }; shift; done
cp "$FAKE/body" "$out"
cat "$FAKE/code"
S
chmod +x "$T/curl"
run() { FAKE=$T CURL="$T/curl" NODE_CONTROL_URL=http://nc sh "$script" "$@" > "$T/out" 2>&1; echo $?; }
check() { # NAME WANT-RC GOT-RC TEXT...
  name=$1 want=$2 got=$3; shift 3
  if [ "$got" != "$want" ]; then echo "FAIL $name: exit $got, want $want"; cat "$T/out"; fails=$((fails + 1)); return; fi
  for t in "$@"; do
    if ! grep -qF -- "$t" "$T/out"; then echo "FAIL $name: no '$t' in:"; cat "$T/out"; fails=$((fails + 1)); return; fi
  done
  echo "ok   $name"
}

echo 200 > "$T/code"
cat > "$T/body" <<'J'
{"node_control":"v0.16.0","headscale":"v0.29.7","tailscale_pin":"1.104.5",
 "nodes":[{"name":"relay-1","online":true,"tags":["tag:relay"],"tailscale":"1.104.5","status":"current"},
          {"name":"phone","online":false,"tailscale":"1.90.0","status":"older"}],
 "bridges":[{"name":"br-1","source":"discovered","online":true,"active":true,"sync":"ok","agent":"0.16.0+abc1234","xray":"25.10.15"}],
 "funnel":{"version":"0.16.0","reported_at":"2026-11-10T10:00:00Z","stale":false,"errors":0},
 "problems":[],"notes":[]}
J
check "healthy fleet exits 0" 0 "$(run)" "node-control" "v0.29.7" "agent 0.16.0+abc1234 xray 25.10.15" "relay-1" "no problems"
if grep -q phone "$T/out"; then echo "FAIL a device is listed without --all"; fails=$((fails + 1)); else echo "ok   devices left out by default"; fi
check "--all lists devices" 0 "$(run --all)" "phone" "older, offline"

jq '.problems = ["bridge br-1: offline"] | .notes = ["bridge x: the agent reports no version (before 0.16.0)"] | .bridges[0].online = false' "$T/body" > "$T/b2" && mv "$T/b2" "$T/body"
check "a problem exits 1" 1 "$(run)" "PROBLEM bridge br-1: offline" "offline" "note: bridge x"

jq '.problems = ["\u001b[31mred"]' "$T/body" > "$T/b2" && mv "$T/b2" "$T/body"
run >/dev/null
if grep -q "$(printf '\033')" "$T/out"; then echo "FAIL escape characters reach the terminal"; fails=$((fails + 1)); else echo "ok   control characters are removed"; fi
check "--json prints the report" 1 "$(run --json)" '"node_control"'

# #300: compatibility shows on the rows, runners are listed, strict mode in the footer
echo 200 > "$T/code"
cat > "$T/body" <<'J'
{"node_control":"v0.18.0","tailscale_pin":"1.104.5","compat_strict":true,"nodes":[],
 "bridges":[{"name":"br-2","source":"discovered","online":true,"active":true,"sync":"ok","agent":"0.19.0","compat":{"interface":"agent-api","state":"incompatible","reported":2}}],
 "funnel":{"version":"0.16.0","reported_at":"2026-11-10T10:00:00Z","stale":false,"errors":0,"compat":{"interface":"funnel-api","state":"unknown"}},
 "runners":[{"address":"100.64.0.8","node":"runner-1","compat":{"interface":"runner-api","state":"incompatible","reported":5,"hint":"newer than node-control accepts: upgrade node-control"}}],
 "problems":["bridge br-2: agent-api version 2 is newer"],"notes":[]}
J
check "compatibility on the rows" 1 "$(run)" "INCOMPATIBLE agent-api 2" "api unknown" "runner-1" "runner-api 5" "incompatible newer than node-control accepts" "COMPAT_STRICT on"

echo 403 > "$T/code"; echo '{"detail":"no"}' > "$T/body"
check "no access exits 2" 2 "$(run)" "HTTP 403"
echo 200 > "$T/code"; echo '<html>' > "$T/body"
check "an old node-control exits 2" 2 "$(run)" "before 0.16.0"
check "an unknown argument exits 2" 2 "$(run --bad)" "unknown argument"

[ "$fails" -eq 0 ] && echo "all fleet-check tests passed" || { echo "$fails failed"; exit 1; }

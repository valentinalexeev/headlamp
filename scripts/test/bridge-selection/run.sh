#!/bin/sh
# Tests for scripts/bridge-selection.sh. No network: curl is a fake on PATH; jq is real.
#   sh scripts/test/bridge-selection/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../bridge-selection.sh"
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq is not installed; the bridge selection client tests need it"
  exit 0
fi
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
BIN="$T/bin"; mkdir -p "$BIN"

# Fake curl: writes $T/export.body to -o and prints $T/export.status; $T/fail makes it fail; logs the URL.
cat > "$BIN/curl" <<'STUB'
#!/bin/sh
out=; url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -w|-H|--max-time) shift ;;
    http*) url=$1 ;;
  esac
  shift
done
echo "$url" >> "$T/calls"
[ -e "$T/fail" ] && { echo "curl: (7) failed" >&2; exit 7; }
cp "$T/export.body" "$out"
cat "$T/export.status"
STUB
chmod +x "$BIN/curl"
export PATH="$BIN:$PATH" T NODE_CONTROL_URL=http://node-control.example.test/

pass=0; fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $*"; }
# check NAME CMD...: ok when CMD succeeds, else bad NAME
check() { _n=$1; shift; if "$@"; then ok; else bad "$_n"; fi; }
eq() { [ "$1" = "$2" ]; }

enroll() {
  cat > "$T/enroll.json" <<'E'
{
  "link_host": "link.example.invalid",
  "urltest": {"tolerance": 80},
  "ingresses": [
    {"name": "ingress-1", "group": "group-a", "host": "ingress-1.example.invalid", "public_key": "PK1", "short_id": "aa", "uuids": "base"},
    {"name": "ingress-2", "host": "ingress-2.example.invalid", "public_key": "PK2", "short_id": "bb", "port": 8443, "uuids": "derived"},
    {"name": "ingress-3", "group": "group-b", "host": "ingress-3.example.invalid", "public_key": "PK3", "short_id": "cc", "uuids": "derived"}
  ]
}
E
}
export_body() { printf '%s' "$1" > "$T/export.body"; echo "${2:-200}" > "$T/export.status"; }
run() { sh "$script" "$@" 2>"$T/err"; }

# pull writes the console values and keeps every other key
enroll
export_body '{"urltest":{"tolerance":300,"interval":"5m"},"order":["ingress-2","ingress-1","ingress-3"],"groups":{"ingress-1":"","ingress-2":"group-b","ingress-3":"group-b"},"drained":["ingress-3"]}'
check "pull exit" run pull --file "$T/enroll.json"
f="$T/enroll.json"
check "urltest" eq "$(jq -c .urltest "$f")" '{"tolerance":300,"interval":"5m"}'
check "order" eq "$(jq -r '[.ingresses[].name]|join(",")' "$f")" "ingress-2,ingress-1,ingress-3"
check "empty group removed" eq "$(jq -r '.ingresses[1] | has("group")' "$f")" false
check "group set" eq "$(jq -r '.ingresses[0].group' "$f")" group-b
check "bridge keys kept" eq "$(jq -c '.ingresses[0] | [.port, .public_key, .host]' "$f")" '[8443,"PK2","ingress-2.example.invalid"]'
check "top-level key kept" eq "$(jq -r .link_host "$f")" link.example.invalid
check "drained bridge kept" eq "$(jq -r '.ingresses|length' "$f")" 3
check "drain note" grep -q "drains bridge ingress-3" "$T/err"
check "export URL" grep -q "^http://node-control.example.test/enroll/selection/export$" "$T/calls"

# diff: no difference now, exit 0; a different console value: exit 1, file unchanged
check "diff after pull" run diff --file "$f"
export_body '{"urltest":{},"order":["ingress-2","ingress-1","ingress-3"],"groups":{},"drained":[]}'
cp "$f" "$T/before"
run diff --file "$f" >/dev/null; rc=$?
check "diff exits 1" eq "$rc" 1
check "diff keeps the file" cmp -s "$f" "$T/before"
# all defaults: the urltest object goes away
run pull --file "$f"
check "default urltest removed" eq "$(jq -r 'has("urltest")' "$f")" false

# a bridge the console does not know stays, at the end
enroll
export_body '{"urltest":{"tolerance":80},"order":["ingress-3"],"groups":{"ingress-3":"group-b"},"drained":[]}'
run pull --file "$T/enroll.json"
check "unknown bridges at the end" eq "$(jq -r '[.ingresses[].name]|join(",")' "$T/enroll.json")" "ingress-3,ingress-1,ingress-2"

# errors leave the file as it is
expect_rc() { _want=$1; shift; run "$@"; _rc=$?; check "exit $_want: $*" eq "$_rc" "$_want"; }
enroll; cp "$T/enroll.json" "$T/before"
export_body '{}' 404
expect_rc 3 pull --file "$T/enroll.json"
check "404 message" grep -q "console is off" "$T/err"
export_body '{}' 403
expect_rc 3 pull --file "$T/enroll.json"
export_body '{"urltest":[]}' 200
expect_rc 3 pull --file "$T/enroll.json"
touch "$T/fail"
expect_rc 3 pull --file "$T/enroll.json"
rm -f "$T/fail"
check "a failed pull keeps the file" cmp -s "$T/enroll.json" "$T/before"
expect_rc 2 pull --file "$T/missing.json"
echo '{"host":"x"}' > "$T/legacy.json"
expect_rc 2 pull --file "$T/legacy.json"
expect_rc 2 push
NODE_CONTROL_URL='' expect_rc 2 pull --file "$T/enroll.json"

echo "bridge-selection: $pass passed, $fail failed"
[ "$fail" = 0 ]

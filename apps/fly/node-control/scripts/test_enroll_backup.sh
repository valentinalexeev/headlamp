#!/bin/sh
# Tests for enroll-backup.sh against a fake `fly` (and, for the sops path, a fake `sops`); real `age` and `jq` are used.
# No network, no Fly. Needs age, age-keygen and jq. Run: sh scripts/test_enroll_backup.sh
set -u
dir=$(cd "$(dirname "$0")" && pwd)
t=$(mktemp -d) || exit 2
trap 'rm -rf "$t"' EXIT
fail=0
check() { # check <name> <test args...>
  name=$1; shift
  if [ "$@" ]; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}
for tool in age age-keygen jq; do command -v "$tool" >/dev/null 2>&1 || { echo "$0: $tool is required" >&2; exit 2; }; done

bin=$t/bin; mkdir -p "$bin"
# Fake fly: `fly ssh console -a <app> -C "<cmd>"`. Records the command and, for an import, the stdin it was given.
cat >"$bin/fly" <<'FAKE'
#!/bin/sh
cmd=
while [ "$#" -gt 0 ]; do [ "$1" = "-C" ] && cmd=$2; shift; done
printf '%s\n' "$cmd" >>"$FAKE_FLY_LOG"
case $cmd in
  *enroll-export*) [ -z "${FAKE_FLY_FAIL:-}" ] || exit 1; cat "$FAKE_EXPORT" ;;
  *enroll-import*) cat >"$FAKE_FLY_STDIN"; echo "imported 2 devices, 0 links, 1 approvals; verified" ;;
  *) exit 3 ;;
esac
FAKE
# Fake sops: -e wraps stdin in a JSON envelope with the payload base64-encoded; -d reads it back.
cat >"$bin/sops" <<'FAKE'
#!/bin/sh
printf '%s\n' "sops $*" >>"$FAKE_FLY_LOG"
case $1 in
  -d) jq -r '.data' "$2" | base64 -d ;;
  -e) printf '{"sops":{"mock":true},"data":"%s"}\n' "$(base64 | tr -d '\n')" ;;
esac
FAKE
chmod +x "$bin/fly" "$bin/sops"
PATH=$bin:$PATH; export PATH

FAKE_FLY_LOG="$t/fly.log"; FAKE_FLY_STDIN="$t/stdin.json"; FAKE_EXPORT="$t/export.json"
export FAKE_FLY_LOG FAKE_FLY_STDIN FAKE_EXPORT
cat >"$FAKE_EXPORT" <<'JSON'
{"seq": 7, "devices": [
  {"id": 1, "name": "secret-phone-name", "owner": "secret-owner", "client": "clash", "expires_at": 4000000000, "revoked": false, "epoch": 0,
   "uuids": {"auto": "11111111-aaaa-4bbb-8ccc-000000000001", "direct": "11111111-aaaa-4bbb-8ccc-000000000002", "exit": "11111111-aaaa-4bbb-8ccc-000000000003"}},
  {"id": 5, "name": "other", "owner": "o", "client": "clash", "expires_at": 4000000000, "revoked": true, "epoch": 2,
   "uuids": {"auto": "22222222-aaaa-4bbb-8ccc-000000000001"}}],
 "approvals": [{"name": "ing", "fingerprint": "fp", "approved_at": 1, "node_id": 3, "machine_key": "mk"}]}
JSON
age-keygen -o "$t/key.txt" >/dev/null 2>&1
recipient=$(grep -o 'age1[a-z0-9]*' "$t/key.txt" | head -1)
run() { sh "$dir/enroll-backup.sh" "$@"; }
exists() { if [ -e "$1" ]; then echo 1; else echo 0; fi; }

# --- argument checks
run >/dev/null 2>&1; check "no arguments is a usage error" $? -eq 2
run frobnicate app f --plaintext >/dev/null 2>&1; check "unknown mode" $? -eq 2
run backup app "$t/o" >/dev/null 2>&1; check "an encryption choice is required" $? -eq 2
run backup -x "$t/o" --plaintext >/dev/null 2>&1; check "an app name starting with - is refused" $? -eq 2
run backup app "$t/o" --age >/dev/null 2>&1; check "--age without a recipient" $? -eq 2
run backup app "$t/o" --age "$recipient" --force >/dev/null 2>&1; check "--force is restore-only" $? -eq 2
run restore app "$t/o" --age "$recipient" >/dev/null 2>&1; check "restore refuses a recipient" $? -eq 2
run restore app "$t/o" --age-identity >/dev/null 2>&1; check "--age-identity without a file" $? -eq 2

# --- backup with age
out=$t/backup.age
msg=$(run backup myapp "$out" --age "$recipient" 2>&1); rc=$?
check "age backup succeeds" $rc -eq 0
check "it reports counts only" "$msg" = "backup written: $out (2 devices, 1 approvals, id counter 7; encryption: age)"
check "the file exists, mode 0600" "$(stat -f %Lp "$out" 2>/dev/null || stat -c %a "$out")" = 600
check "no device UUID or name in the encrypted file" "$(grep -c 'aaaa-4bbb\|secret-phone-name\|secret-owner' "$out")" = 0
check "the message does not leak names" "$(printf '%s' "$msg" | grep -c 'secret')" = 0
check "it asked the app to export" "$(grep -c 'node-control enroll-export' "$FAKE_FLY_LOG")" -ge 1
check "no temporary files left behind" "$(find "$t" -maxdepth 1 -name '.enroll-backup.*' | wc -l | tr -d ' ')" = 0

# --- an existing file is never overwritten
run backup myapp "$out" --age "$recipient" >/dev/null 2>&1; check "refuses to overwrite" $? -eq 2
check "the existing file is untouched" "$(age -d -i "$t/key.txt" "$out" | jq '.seq')" = 7

# --- restore from that backup
: >"$FAKE_FLY_LOG"
msg=$(run restore myapp "$out" --age-identity "$t/key.txt" 2>&1); rc=$?
check "restore succeeds" $rc -eq 0
check "the app received exactly the exported JSON on stdin" "$(jq -S . "$FAKE_FLY_STDIN")" = "$(jq -S . "$FAKE_EXPORT")"
check "it ran enroll-import -json -" "$(head -1 "$FAKE_FLY_LOG")" = "node-control enroll-import -json -"
: >"$FAKE_FLY_LOG"
run restore myapp "$out" --age-identity "$t/key.txt" --force >/dev/null 2>&1
check "--force is passed on" "$(head -1 "$FAKE_FLY_LOG")" = "node-control enroll-import -json - -force"

# --- bad inputs never reach the app
: >"$FAKE_FLY_LOG"
printf 'not an export' >"$t/garbage"
run restore myapp "$t/garbage" --plaintext >/dev/null 2>&1; check "garbage restore refused" $? -eq 2
check "…and fly was not called" "$(wc -c <"$FAKE_FLY_LOG" | tr -d ' ')" = 0
run restore myapp "$t/missing" --plaintext >/dev/null 2>&1; check "missing file refused" $? -eq 2
printf '{"seq":1,"devices":[],"approvals":[]}' >"$t/empty-export"
cp "$FAKE_EXPORT" "$t/real-export"
printf 'not json at all' >"$FAKE_EXPORT"
run backup myapp "$t/x1" --age "$recipient" >/dev/null 2>&1; check "a non-JSON export writes nothing" "$(exists "$t/x1")" = 0
cp "$t/empty-export" "$FAKE_EXPORT"
run backup myapp "$t/x2" --age "$recipient" >/dev/null 2>&1; check "an export with no devices is refused" "$(exists "$t/x2")" = 0
(FAKE_FLY_FAIL=1; export FAKE_FLY_FAIL; run backup myapp "$t/x3" --age "$recipient" >/dev/null 2>&1); check "a failing fly writes nothing" "$(exists "$t/x3")" = 0
cp "$t/real-export" "$FAKE_EXPORT"
check "no temporary files after failures" "$(find "$t" -maxdepth 1 -name '.enroll-backup.*' | wc -l | tr -d ' ')" = 0

# --- plaintext only when asked, mode 0600
run backup myapp "$t/plain.json" --plaintext >/dev/null 2>&1
check "plaintext backup equals the export" "$(jq -S . "$t/plain.json")" = "$(jq -S . "$FAKE_EXPORT")"
check "plaintext file is mode 0600" "$(stat -f %Lp "$t/plain.json" 2>/dev/null || stat -c %a "$t/plain.json")" = 600

# --- sops path (fake sops): encrypted from stdin with the file name for the creation rules, and round-trips
: >"$FAKE_FLY_LOG"
run backup myapp "$t/enroll.sops.json" --sops >/dev/null 2>&1; check "sops backup succeeds" $? -eq 0
check "sops got the destination name for the creation rules" "$(grep -c -- "--filename-override $t/enroll.sops.json" "$FAKE_FLY_LOG")" -ge 1
check "…and JSON types, reading stdin" "$(grep -c -- '--input-type json --output-type json' "$FAKE_FLY_LOG")" -ge 1
check "no UUID in the sops output" "$(grep -c 'aaaa-4bbb' "$t/enroll.sops.json")" = 0
: >"$FAKE_FLY_LOG"
run restore myapp "$t/enroll.sops.json" --sops >/dev/null 2>&1
check "sops restore round-trips" "$(jq -S . "$FAKE_FLY_STDIN")" = "$(jq -S . "$FAKE_EXPORT")"

[ "$fail" -eq 0 ] && echo "all enroll-backup tests passed"
exit "$fail"

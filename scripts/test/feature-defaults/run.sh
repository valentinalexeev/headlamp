#!/usr/bin/env bash
# Tests for scripts/check-feature-defaults.sh.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
chk=$here/../../check-feature-defaults.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
mkdir -p "$t/roles/r1/defaults"; : > "$t/allow"
fail=0
expect() { if [ "$1" = "$2" ]; then echo "ok: $3"; else echo "FAIL: $3 (rc $2)"; fail=1; fi; }
printf 'x_enabled: false\ny: true\n' > "$t/roles/r1/defaults/main.yml"
rc=0; "$chk" "$t/roles" "$t/allow" >/dev/null 2>&1 || rc=$?; expect 0 "$rc" "off default passes"
printf 'x_enabled: true\n' > "$t/roles/r1/defaults/main.yml"
rc=0; "$chk" "$t/roles" "$t/allow" >/dev/null 2>&1 || rc=$?; expect 1 "$rc" "new on default fails"
printf 'x_enabled: "yes"  # c\n' > "$t/roles/r1/defaults/main.yml"
rc=0; "$chk" "$t/roles" "$t/allow" >/dev/null 2>&1 || rc=$?; expect 1 "$rc" "quoted yes fails"
echo 'r1 x_enabled' > "$t/allow"
rc=0; "$chk" "$t/roles" "$t/allow" >/dev/null 2>&1 || rc=$?; expect 0 "$rc" "allowlisted passes"
rc=0; "$chk" >/dev/null 2>&1 || rc=$?; expect 0 "$rc" "repo tree passes"
exit "$fail"

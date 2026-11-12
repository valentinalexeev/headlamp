#!/bin/sh
# On-demand check of the env file quoting rule of examples/compose/control (#302). Run it with
# `make test-compose-env`. Needs docker compose.
#
# Docker Compose expands "$name" in env_file values. The README says to write each secret value in single quotes, or
# each "$" as "$$". This test starts a container with an env file and checks what the container gets:
#   - a single-quoted value and a "$$" value reach the container unchanged;
#   - an unquoted and a double-quoted value are shortened (so the warning in the README is still true).
# It also checks that each value of the *.env.example files that holds a "$" is single-quoted.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok - $1"; }
bad() { fail=$((fail + 1)); echo "not ok - $1"; }

if ! docker compose version >/dev/null 2>&1; then
  echo "# skip: docker compose is not available"
  exit 0
fi

T=$(mktemp -d "${TMPDIR:-/tmp}/compose-env.XXXXXX")
proj="compose-env-$$"
cleanup() {
  docker compose -p "$proj" --project-directory "$T" down -q >/dev/null 2>&1 || true
  rm -rf "$T"
}
trap cleanup EXIT INT TERM

want='ab$cdef$1gh'
cat > "$T/compose.yaml" <<'EOF'
services:
  t:
    image: busybox:1.37
    env_file: ./t.env
    command: ["sh", "-c", "printf '%s\\n' \"$$SQ\" \"$$DD\" \"$$BARE\" \"$$DQ\""]
EOF
# The values are written with printf, so the shell of this test does not expand them.
{
  printf '%s\n' "SQ='$want'"
  printf '%s\n' 'DD=ab$$cdef$$1gh'
  printf '%s\n' "BARE=$want"
  printf '%s\n' "DQ=\"$want\""
} > "$T/t.env"

out=$(docker compose -p "$proj" --project-directory "$T" run --rm -q t 2>/dev/null) || {
  echo "not ok - docker compose run"
  exit 1
}
sq=$(printf '%s\n' "$out" | sed -n 1p)
dd=$(printf '%s\n' "$out" | sed -n 2p)
bare=$(printf '%s\n' "$out" | sed -n 3p)
dq=$(printf '%s\n' "$out" | sed -n 4p)

[ "$sq" = "$want" ] && ok "a single-quoted value reaches the container unchanged" \
  || bad "single-quoted: got '$sq', want '$want'"
[ "$dd" = "$want" ] && ok "a value with \$\$ reaches the container unchanged" \
  || bad "\$\$: got '$dd', want '$want'"
[ "$bare" != "$want" ] && ok "an unquoted value is shortened ('$bare'): the README warning holds" \
  || bad "unquoted value was not changed: update the README warning"
[ "$dq" != "$want" ] && ok "a double-quoted value is shortened ('$dq'): the README warning holds" \
  || bad "double-quoted value was not changed: update the README warning"

# Each example value with a "$" must be single-quoted.
for f in "$root"/examples/compose/control/*.env.example; do
  n=$(grep -vE '^[[:space:]]*#' "$f" | grep -E '^[A-Z0-9_]+=.*\$' | grep -cvE "^[A-Z0-9_]+='[^']*'[[:space:]]*$" || true)
  [ "$n" -eq 0 ] && ok "$(basename "$f"): no unquoted \$ in a value" || bad "$(basename "$f"): $n values with an unquoted \$"
done

echo "# $pass passed, $fail failed"
[ "$fail" -eq 0 ]

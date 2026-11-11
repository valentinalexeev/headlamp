#!/usr/bin/env bash
# Tests for scripts/check-compat.sh (#300) in a throwaway git repo. No network.
#   bash scripts/test/check-compat/run_tests.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../check-compat.sh"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
fails=0
ok()  { echo "ok   $1"; }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }

cd "$T" || exit 1
git init -q -b main . && git config user.email t@t && git config user.name t
mkdir -p apps/node-control/internal/compat apps/agent apps/runner
cat > apps/node-control/internal/compat/compat.json <<'J'
{"interfaces":{
 "agent-api":{"between":["node-control","agent"],"current":1,"accepted":{"min":1,"max":1},"covers":["apps/agent/api.go"]},
 "runner-api":{"between":["node-control","runner"],"current":1,"accepted":{"min":1,"max":1},"covers":["apps/runner/api.go"]}}}
J
echo a > apps/agent/api.go; echo r > apps/runner/api.go; echo x > other.txt
git add -A && git commit -qm base && git tag base
run() { bash "$script" base HEAD > "$T/out" 2>&1; echo $?; }

echo y > other.txt; git commit -qam "fix: other"
[ "$(run)" = 0 ] && ok "a change outside the interfaces passes" || { nok "outside"; cat "$T/out"; }

echo a2 > apps/agent/api.go; git commit -qam "feat: agent api"
[ "$(run)" = 1 ] && grep -q "agent-api interface and changed" "$T/out" && ok "an interface change without the matrix fails" || { nok "no matrix"; cat "$T/out"; }

git commit -q --allow-empty -m "docs: note" -m "compat: unchanged agent-api"
[ "$(run)" = 0 ] && grep -q "declared unchanged" "$T/out" && ok "a declared unchanged interface passes" || { nok "declared"; cat "$T/out"; }

echo r2 > apps/runner/api.go; git commit -qam "feat: runner api"
[ "$(run)" = 1 ] && grep -q "runner-api" "$T/out" && ! grep -q "agent-api interface and changed" "$T/out" && ok "the declaration covers only its interface" || { nok "scope"; cat "$T/out"; }

sed -i.bak 's/"runner-api":{"between":\["node-control","runner"\],"current":1,"accepted":{"min":1,"max":1}/"runner-api":{"between":["node-control","runner"],"current":2,"accepted":{"min":1,"max":2}/' apps/node-control/internal/compat/compat.json && rm -f apps/node-control/internal/compat/compat.json.bak
git commit -qam "feat: runner api 2"
[ "$(run)" = 0 ] && ok "a matrix change with the interfaces passes" || { nok "with matrix"; cat "$T/out"; }

git rm -q apps/runner/api.go && git commit -qm "refactor: move runner api"
[ "$(run)" = 1 ] && grep -q "which does not exist" "$T/out" && ok "a covered file that is gone fails" || { nok "missing file"; cat "$T/out"; }

[ "$fails" = 0 ] && echo "all check-compat tests passed" || { echo "$fails failed"; exit 1; }

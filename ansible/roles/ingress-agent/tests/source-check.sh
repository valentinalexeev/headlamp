#!/usr/bin/env bash
# Runs the "agent source" preflight of the ingress-agent role (#144) with real ansible against the real
# apps/ingress-agent directory, so a stale path in the role fails CI. Also checks that a directory without the Go
# sources is refused, and that check mode does not skip the check.
# Usage: ansible/roles/ingress-agent/tests/source-check.sh
set -u
if ! command -v ansible-playbook >/dev/null 2>&1; then
  echo "SKIP: ansible-playbook is not installed"
  exit 0
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo="$(cd "$here/../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# ansible's own interpreter has PyYAML; the system python3 may not.
py="$(head -1 "$(command -v ansible-playbook)" | sed -n 's/^#! *//p' | awk '{print $NF}')"
case "$py" in */python*) ;; *) py=python3 ;; esac
"$py" - "$here/tasks/preflight.yml" "$tmp/src.yml" <<'PY' || { echo "FAIL: cannot extract the source tasks"; exit 1; }
import sys, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
src = [t for t in tasks if "agent source" in t.get("name", "")]
assert len(src) >= 2, [t.get("name") for t in tasks]
yaml.safe_dump([{"hosts": "localhost", "gather_facts": False, "tasks": src}], open(sys.argv[2], "w"))
PY
fails=0
run() { # NAME EXPECT(pass|fail) SRC [EXTRA ARGS]
  local name="$1" expect="$2" src="$3"; shift 3
  local out rc got=pass ok=1
  out="$(ansible-playbook "$tmp/src.yml" -i localhost, -c local -e "ingress_agent_src=$src" "$@" 2>&1)"; rc=$?
  [ $rc -ne 0 ] && got=fail
  [ "$got" = "$expect" ] || ok=0
  if [ "$expect" = fail ] && ! echo "$out" | grep -q "ingress-agent: .*cmd/ingress-agent"; then ok=0; fi
  if [ $ok = 1 ]; then echo "ok   $name"; else echo "FAIL $name (got $got)"; echo "$out" | tail -5; fails=$((fails+1)); fi
}
run "the real apps/ingress-agent passes" pass "$repo/apps/ingress-agent"
run "the real apps/ingress-agent passes in check mode" pass "$repo/apps/ingress-agent" --check
mkdir -p "$tmp/python-only/app" && touch "$tmp/python-only/app/main.py"
run "a tree with only the old Python agent fails" fail "$tmp/python-only"
mkdir -p "$tmp/no-cmd" && cp "$repo/apps/ingress-agent/go.mod" "$tmp/no-cmd/"
run "go.mod without cmd/ingress-agent fails" fail "$tmp/no-cmd"
run "a missing directory fails" fail "$tmp/does-not-exist"
run "a missing directory fails in check mode" fail "$tmp/does-not-exist" --check
[ $fails = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

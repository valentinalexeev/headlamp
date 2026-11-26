#!/usr/bin/env bash
# Checks that the commented path check settings in the private repo example (#425) pass the "Check the path check
# settings" preflight of the ingress-agent role, with real ansible, once they are uncommented.
# Usage: ansible/roles/ingress-agent/tests/example-pathcheck.sh
set -u
if ! command -v ansible-playbook >/dev/null 2>&1; then
  echo "SKIP: ansible-playbook is not installed"
  exit 0
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo="$(cd "$here/../../.." && pwd)"
example="$repo/examples/private-repo/ansible/host_vars/bridge1.yml.example"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# ansible's own interpreter has PyYAML; the system python3 may not.
py="$(head -1 "$(command -v ansible-playbook)" | sed -n 's/^#! *//p' | awk '{print $NF}')"
case "$py" in */python*) ;; *) py=python3 ;; esac
"$py" - "$here/tasks/preflight.yml" "$here/defaults/main.yml" "$example" "$tmp/play.yml" "$tmp/vars.yml" <<'PY' || { echo "FAIL: cannot build the test play"; exit 1; }
import re, sys, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
check = [t for t in tasks if t.get("name") == "Check the path check settings"]
assert len(check) == 1
defaults = {k: v for k, v in yaml.safe_load(open(sys.argv[2])).items() if k.startswith("ingress_pathcheck_")}
lines = [re.sub(r"^# ", "", l) for l in open(sys.argv[3]) if re.match(r"^# ingress_pathcheck_[a-z_]+:", l)]
example = yaml.safe_load("".join(lines)) or {}
assert example.get("ingress_pathcheck_target"), "the example has no commented ingress_pathcheck_target"
defaults.update(example)
yaml.safe_dump(defaults, open(sys.argv[5], "w"))
yaml.safe_dump([{"hosts": "localhost", "gather_facts": False, "tasks": check}], open(sys.argv[4], "w"))
PY
if out="$(ansible-playbook "$tmp/play.yml" -i localhost, -c local -e "@$tmp/vars.yml" 2>&1)"; then
  echo "ok   example path check settings pass the preflight"
else
  echo "$out"
  echo "FAIL example path check settings fail the preflight"
  exit 1
fi

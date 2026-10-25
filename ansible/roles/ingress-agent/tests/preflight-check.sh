#!/usr/bin/env bash
# Checks the /v1/info preflight gate of the ingress-agent role and the env.j2 export with real ansible.
# Usage: ansible/roles/ingress-agent/tests/preflight-check.sh
set -u
if ! command -v ansible-playbook >/dev/null 2>&1; then
  echo "SKIP: ansible-playbook is not installed"
  exit 0
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap '[ -n "${KEEP:-}" ] || rm -rf "$tmp"' EXIT
python3 - "$here/tasks/preflight.yml" "$tmp/pre.yml" <<'PY' || { echo "FAIL: cannot extract the preflight task"; exit 1; }
import sys, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
gate = [t for t in tasks if t.get("name", "").startswith("Validate the public parameters")]
assert len(gate) == 1
yaml.safe_dump([{"hosts": "localhost", "gather_facts": False, "tasks": gate}], open(sys.argv[2], "w"))
PY
cat > "$tmp/render.yml" <<EOF2
- hosts: localhost
  gather_facts: false
  tasks:
    - ansible.builtin.template: {src: "$here/templates/env.j2", dest: "{{ out }}"}
EOF2
fails=0
# run NAME EXPECT(pass|fail) EXTRA_VARS_JSON [EXPECT_LINES(none|six|-)]
run() {
  local name="$1" expect="$2" vars="$3" lines="${4:--}" out="$tmp/env.out"
  local base='{"ingress_name":"","ingress_host":"","reality_public_key":"","reality_short_id":"","ingress_listen_port":443,"reality_sni":"{{ ingress_host }}"'
  local all="${base},${vars#\{}"; all="${all%,}"; [ "$vars" = "{}" ] && all="${base}}"
  local pre rc
  pre="$(ansible-playbook "$tmp/pre.yml" -i localhost, -c local -e "$all" 2>&1)"; rc=$?
  local got=pass; [ $rc -ne 0 ] && got=fail
  local ok=1
  [ "$got" = "$expect" ] || ok=0
  if [ "$expect" = fail ]; then
    # must name a variable and must not echo values
    echo "$pre" | grep -q "ingress-agent: [a-z_]* " || ok=0
  fi
  if [ "$lines" != "-" ] && [ "$got" = pass ]; then
    rm -f "$out"
    ansible-playbook "$tmp/render.yml" -i localhost, -c local -e "$all" -e "out=$out" \
      -e ingress_agent_token=x -e ingress_agent_state_dir=/s -e xray_base_config=/b -e xray_config=/c -e xray_bin=/x \
      -e xray_unit=x -e xray_api=a -e xray_asset_dir=/d -e '{"ingress_direct_geoip":[],"ingress_direct_geosite":[]}' >"$tmp/render.log" 2>&1
    local n; n="$(grep -c '^INGRESS_\(NAME\|HOST\|PORT\|SNI\|PUBLIC_KEY\|SHORT_ID\)=' "$out" 2>/dev/null || true)"; n="${n:-0}"
    if [ "$lines" = none ] && [ "$n" != 0 ]; then ok=0; fi
    if [ "$lines" = six ] && [ "$n" != 6 ]; then ok=0; fi
  fi
  if [ $ok = 1 ]; then echo "ok   $name"; else echo "FAIL $name (preflight=$got, expected $expect)"; fails=$((fails+1)); fi
}
HOST='"ingress_host":"relay.example.com","reality_short_id":"0123abcd"'
ALL='"ingress_name":"ingress-1","ingress_host":"relay.example.com","reality_short_id":"0123abcd","reality_public_key":"fakeKey_-1="'
# (1) only the sibling role's required variables: passes, nothing exported
run "1 host+short_id only passes, no INGRESS_ lines" pass "{$HOST}" none
run "1b nothing set passes" pass "{}" none
# (2) all valid
run "2 all valid passes, six lines" pass "{$ALL}" six
# (3) partial configurations
run "3a name without public key fails" fail "{$HOST,\"ingress_name\":\"ingress-1\"}"
run "3b public key without name fails" fail "{$HOST,\"reality_public_key\":\"fakeKey\"}"
run "3c name+key without host fails" fail '{"ingress_name":"ingress-1","reality_public_key":"fakeKey","reality_short_id":"0123abcd"}'
run "3d name+key without short id fails" fail '{"ingress_name":"ingress-1","reality_public_key":"fakeKey","ingress_host":"relay.example.com"}'
# (4) hostile values in each variable while the gate is active
for bad in $'a\nb' 'a#b' 'a"b' "a'b" 'a b'; do
  esc="$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1])[1:-1])' "$bad")"
  for var in ingress_name ingress_host reality_public_key reality_short_id reality_sni; do
    v="{$ALL,\"$var\":\"$esc\"}"
    run "4 $var=$(printf %q "$bad") fails" fail "$v"
  done
done
# (5) gate off (no name, no public key): odd short id/host do not matter to this role's gate
run "5 gate off passes with odd host/short_id" pass '{"ingress_host":"relay.example.com","reality_short_id":"AB cd"}' none
[ $fails = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

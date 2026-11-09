#!/usr/bin/env bash
# Tests for scripts/fleet-run.sh (#199) with real ansible and no containers: "good" runs locally, "dead" is an address
# in TEST-NET-1 that never answers, and "bad" is local but its task fails.
#   bash scripts/test/fleet-run/run_tests.sh
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
command -v ansible-playbook >/dev/null || { echo "SKIP: ansible-playbook"; exit 0; }
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT; cd "$T" || exit 1
pass=0; fails=0
ok()  { echo "ok   $1"; pass=$((pass + 1)); }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }
py="$(command -v python3)"
mkdir -p inventory
cat > inventory/hosts.yml <<EOF
all:
  vars: { ansible_python_interpreter: "$py" }
  children:
    exit_nodes_vm:
      hosts:
        good:  { ansible_connection: local }
        good2: { ansible_connection: local }
        bad:   { ansible_connection: local, must_fail: true }
        dead:  { ansible_host: 192.0.2.10, ansible_user: nobody, ansible_ssh_common_args: "-o ConnectTimeout=3" }
EOF
cat > play.yml <<'EOF'
- hosts: exit_nodes_vm
  gather_facts: false
  tasks:
    - ansible.builtin.fail: { msg: "boom" }
      when: must_fail | default(false)
    - ansible.builtin.copy: { content: "x", dest: "{{ playbook_dir }}/ran-{{ inventory_hostname }}" }
      delegate_to: localhost
EOF
fr() { PROBE_TIMEOUT=3 "$root/scripts/fleet-run.sh" play.yml; }

start=$(date +%s); out="$(fr 2>&1)"; rc=$?; took=$(( $(date +%s) - start ))
[ $rc = 2 ] && ok "pending hosts: exit 2" || nok "rc=$rc"
printf '%s' "$out" | grep -q "^done: good good2$" && ok "done: good good2" || nok "done line: $(printf '%s' "$out" | grep '^done')"
printf '%s' "$out" | grep -q "^pending: dead (unreachable" && ok "pending: dead (unreachable ...)" || nok "dead: $(printf '%s' "$out" | grep pending)"
printf '%s' "$out" | grep -q "^pending: bad (a task failed" && ok "pending: bad (a task failed)" || nok "bad: $(printf '%s' "$out" | grep pending)"
[ -f ran-good ] && [ -f ran-good2 ] && [ ! -f ran-dead ] && ok "the playbook ran on the reachable hosts only" || nok "ran: $(ls ran-* 2>/dev/null)"
[ "$took" -lt 60 ] && ok "a dead node does not block the run (${took}s)" || nok "took ${took}s"
[ "$(sort .fleet-pending | cut -d' ' -f1 | tr '\n' ' ')" = "bad dead " ] && ok ".fleet-pending lists bad and dead" || nok "pending file: $(cat .fleet-pending)"

rm -f ran-*
sed -i.b 's/must_fail: true/must_fail: false/; s/192.0.2.10, ansible_user: nobody, ansible_ssh_common_args: "-o ConnectTimeout=3"/127.0.0.1, ansible_connection: local/' inventory/hosts.yml
out="$(ONLY_PENDING=1 fr 2>&1)"; rc=$?
[ $rc = 0 ] && ok "ONLY_PENDING=1 after the nodes are back: exit 0" || nok "retry rc=$rc: $(printf '%s' "$out" | tail -3)"
[ -f ran-bad ] && [ -f ran-dead ] && [ ! -f ran-good ] && ok "the retry runs only the pending hosts" || nok "retry ran: $(ls ran-* 2>/dev/null)"
[ ! -e .fleet-pending ] && ok "no pending file when all are done" || nok "pending file left: $(cat .fleet-pending)"

out="$(EXCLUDE=good2,bad fr 2>&1)"; rc=$?
[ $rc = 2 ] && printf '%s' "$out" | grep -q "^pending: good2 (excluded)" && printf '%s' "$out" | grep -q "^pending: bad (excluded)" \
  && ok "EXCLUDE=good2,bad: both pending (excluded), not run" || nok "exclude rc=$rc: $(printf '%s' "$out" | grep -E '^(done|pending)')"
printf '%s' "$out" | grep -q "^done: dead good$" && ok "EXCLUDE: the others are done" || nok "exclude done: $(printf '%s' "$out" | grep '^done')"

out="$("$root/scripts/fleet-run.sh" nope.yml 2>&1)"; [ $? = 1 ] && ok "no playbook: exit 1" || nok "no playbook: $out"

# "Done when" of #199: `make bump-tailscale-version` with one node down finishes for the others and lists it.
# A site repo with headlamp as the submodule; the bump playbook is replaced by the test play (the tailscale role needs
# a real host), and FLY_EXIT_NODE=0.
export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
core="$T/core"; mkdir -p "$core"
(cd "$root" && git ls-files -co --exclude-standard -z | while IFS= read -r -d '' f; do [ -f "$f" ] && printf '%s\0' "$f"; done \
  | tar --null -T - -cf -) | tar -xf - -C "$core"
cp "$T/play.yml" "$core/ansible/playbooks/bump-tailscale-version.yml"
git -C "$core" init -q && git -C "$core" add -A && git -C "$core" commit -qm core
site="$T/site"; mkdir -p "$site/ansible" && cd "$site" || exit 1
git init -q && git -c protocol.file.allow=always submodule -q add "$core" vendor/headlamp
cp -R "$T/inventory" ansible/ && sed -i.b 's/must_fail: false/must_fail: false/; s/127.0.0.1, ansible_connection: local/192.0.2.10, ansible_ssh_common_args: "-o ConnectTimeout=3"/' ansible/inventory/hosts.yml
printf 'H := vendor/headlamp\ninclude $(H)/mk/site.mk\n' > Makefile && echo 1.104.5 > TAILSCALE_VERSION
git add -A && git commit -qm site
out="$(PROBE_TIMEOUT=3 make -s bump-tailscale-version VERSION=1.104.6 FLY_EXIT_NODE=0 2>&1)"; rc=$?
[ $rc = 0 ] && ok "make bump-tailscale-version with a node down: finishes (rc 0)" || nok "bump rc=$rc: $(printf '%s' "$out" | tail -4)"
printf '%s' "$out" | grep -q "^pending: dead (unreachable" && printf '%s' "$out" | grep -q "^done: .*good" \
  && ok "the bump lists done nodes and 'pending: dead (unreachable ...)'" || nok "bump output: $(printf '%s' "$out" | grep -E '^(done|pending)')"
[ "$(cat TAILSCALE_VERSION)" = 1.104.6 ] && ok "the pin is written" || nok "pin is $(cat TAILSCALE_VERSION)"

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

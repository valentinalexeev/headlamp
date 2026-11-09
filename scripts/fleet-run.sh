#!/usr/bin/env bash
# Fleet run of an Ansible playbook that does not wait for nodes that are down (#199). It probes the hosts first, with a
# short timeout, runs the playbook only on the hosts that answer, and ends with a list of what is done and what is
# pending, with the reason. A stopped on-demand node or a node with no cloud capacity no longer blocks the others.
#
#   fleet-run.sh <playbook> [ansible-playbook args...]
#
# Environment:
#   INVENTORY      inventory path (default inventory/hosts.yml, relative to the current directory)
#   HOSTS          host pattern of the run (default all; the plays still pick their own groups)
#   EXCLUDE        hosts to skip, comma or space separated (reported as pending: excluded)
#   PROBE_TIMEOUT  seconds for the reachability probe of each host (default 15). 0 skips the probe.
#   PENDING_FILE   where to write the pending hosts, one "host reason" per line (default .fleet-pending)
#   ONLY_PENDING=1 run only the hosts in PENDING_FILE (the retry after a node is back)
# Exit: 0 when every host is done, 2 when some are pending, 1 on a usage or setup error.
set -uo pipefail
pb="${1:?usage: fleet-run.sh <playbook> [ansible-playbook args...]}"; shift
inv="${INVENTORY:-inventory/hosts.yml}"
pattern="${HOSTS:-all}"
probe_t="${PROBE_TIMEOUT:-15}"
pfile="${PENDING_FILE:-.fleet-pending}"
die() { echo "fleet-run: $*" >&2; exit 1; }
[ -f "$pb" ] || die "no playbook $pb"
command -v ansible-playbook >/dev/null || die "ansible-playbook is not installed"

# The hosts that the playbook would touch.
mapfile -t all < <(ansible-playbook -i "$inv" -l "$pattern" --list-hosts "$pb" "$@" 2>/dev/null \
  | awk '/hosts \([0-9]+\):/{f=1; next} f && NF==1 {print $1} /^ *play #/{f=0}' | sort -u)
[ "${#all[@]}" -gt 0 ] || die "the playbook matches no hosts in $inv"

declare -A reason=()
if [ "${ONLY_PENDING:-}" = 1 ]; then
  [ -s "$pfile" ] || die "ONLY_PENDING=1 but $pfile is empty or missing"
  declare -A want=(); while read -r h _; do [ -n "$h" ] && want[$h]=1; done < "$pfile"
  for h in "${all[@]}"; do [ -n "${want[$h]:-}" ] || reason[$h]="skip"; done
fi
exclude="${EXCLUDE:-}"
for h in ${exclude//,/ }; do [ -n "$h" ] && reason[$h]="excluded"; done

run=(); for h in "${all[@]}"; do [ -z "${reason[$h]:-}" ] && run+=("$h"); done

# Probe: a host that does not answer within PROBE_TIMEOUT is pending, and the playbook does not wait for it.
if [ "${#run[@]}" -gt 0 ] && [ "$probe_t" != 0 ]; then
  lim="$(IFS=,; echo "${run[*]}")"
  while IFS= read -r line; do
    h="${line%% |*}"
    case "$line" in
      *" | SUCCESS"*) ;;
      *" | UNREACHABLE!"*) reason[$h]="unreachable (no answer in ${probe_t}s: stopped, starting, or no capacity)" ;;
      *" | FAILED!"*) reason[$h]="probe failed (python or login on the host)" ;;
    esac
  done < <(ANSIBLE_TIMEOUT="$probe_t" ansible -i "$inv" -l "$lim" all -m ansible.builtin.ping -o -T "$probe_t" 2>/dev/null)
  run=(); for h in "${all[@]}"; do [ -z "${reason[$h]:-}" ] && run+=("$h"); done
fi

rc=0
if [ "${#run[@]}" -gt 0 ]; then
  lim="$(IFS=,; echo "${run[*]}")"
  echo "fleet-run: running $pb on ${#run[@]} host(s): ${run[*]}"
  log="$(mktemp)"; trap 'rm -f "$log"' EXIT
  ansible-playbook -i "$inv" -l "$lim" "$pb" "$@" 2>&1 | tee "$log"; rc=${PIPESTATUS[0]}
  # The recap: a host with failed or unreachable tasks is pending.
  while read -r h f u; do
    [ "$u" -gt 0 ] && reason[$h]="unreachable during the run" && continue
    [ "$f" -gt 0 ] && reason[$h]="a task failed (see the output above)"
  done < <(sed -n '/^PLAY RECAP/,$p' "$log" | awk '/ : ok=/{h=$1; for(i=2;i<=NF;i++){split($i,a,"="); v[a[1]]=a[2]} print h, v["failed"]+0, v["unreachable"]+0}')
else
  echo "fleet-run: no host to run on"
fi

done_=(); pending=()
for h in "${all[@]}"; do
  case "${reason[$h]:-}" in
    "") done_+=("$h") ;;
    skip) ;;
    *) pending+=("$h") ;;
  esac
done
: > "$pfile"
echo
echo "done: ${done_[*]:-(none)}"
if [ "${#pending[@]}" -gt 0 ]; then
  for h in "${pending[@]}"; do echo "pending: $h (${reason[$h]})"; echo "$h ${reason[$h]}" >> "$pfile"; done
  echo "Retry them later with ONLY_PENDING=1 (the list is in $pfile)."
  exit 2
fi
rm -f "$pfile"
# All hosts done but ansible still failed (for example a syntax error): pass its code on.
[ "$rc" = 0 ] || exit "$rc"

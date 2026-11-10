#!/bin/sh
# Apply a changed headscale policy (config/acl.json) on the host of examples/compose/control (#261).
#
#   scripts/policy-apply.sh [--check-only]
#
# In file mode, `docker compose up -d` does not reload a changed acl.json: it is a bind mount, so compose sees no
# change and the old policy stays active. A policy that passes CI can also stop headscale at the next restart,
# because CI checks it against test users, and the live users can make a reference ambiguous (#253).
#
# This script, run from the compose directory:
#   1. checks the new file with `headscale policy check` inside the running headscale, against the live users and
#      nodes. The tests block is left out, because its sources need the tagged nodes of CI;
#   2. reloads the policy with SIGHUP (no restart, no lost connections);
#   3. confirms the reload in the headscale log ("Policy reload completed"). In file mode `headscale policy get`
#      reads the file from disk, not the loaded policy, so it cannot confirm a reload.
#      headscale 0.29 refuses a SIGHUP reload when a test of the tests block fails (for example a tag with no live
#      node yet), but only warns about it at start. Step 1 passed without the tests, so in that case the script
#      restarts headscale, and confirms that it is healthy again.
#
# A single-file bind mount pins the inode of the file. `git pull`, `sed -i` and most editors write a new file, and
# then the container still sees the old one, also after SIGHUP or a restart. Before step 2 the script compares the
# file in the container with config/acl.json, and recreates the container (up -d --force-recreate) when they differ.
# On a failed check it stops before step 2, so headscale keeps the old policy.
#
# Env: COMPOSE (default "docker compose"), SERVICE (default headscale), POLICY (default config/acl.json).
set -eu
compose=${COMPOSE:-docker compose}
svc=${SERVICE:-headscale}
policy=${POLICY:-config/acl.json}
check_only=false
[ "${1:-}" = --check-only ] && check_only=true

die() { echo "policy-apply: $*" >&2; exit 1; }
[ -f "$policy" ] || die "$policy not found (run this from the compose directory)"
command -v python3 >/dev/null 2>&1 || die "python3 is needed (to read HuJSON)"
# shellcheck disable=SC2086 # COMPOSE may be two words
hs() { $compose exec -T "$svc" headscale "$@"; }
hs users list >/dev/null 2>&1 || die "headscale ($svc) is not running; start it with: $compose up -d $svc"

# HuJSON -> JSON without the tests block (headscale policy check needs the tagged nodes of CI for the tests).
canon() {
  python3 - "$1" "$2" <<'PY'
import json, re, sys
src, drop_tests = sys.argv[1], sys.argv[2] == "1"
s = open(src).read()
out, i, n, instr = [], 0, len(s), False
while i < n:  # strip // and /* */ comments outside strings
    c = s[i]
    if instr:
        out.append(c)
        if c == "\\": out.append(s[i + 1]); i += 1
        elif c == '"': instr = False
    elif c == '"': instr = True; out.append(c)
    elif s.startswith("//", i): i = s.find("\n", i); i = n if i < 0 else i; continue
    elif s.startswith("/*", i): i = s.index("*/", i) + 2; continue
    else: out.append(c)
    i += 1
s = re.sub(r",(\s*[}\]])", r"\1", "".join(out))  # trailing commas
p = json.loads(s)
if drop_tests: p.pop("tests", None)
print(json.dumps(p, sort_keys=True))
PY
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
canon "$policy" 1 > "$tmp/new.json" || die "$policy is not valid HuJSON"

echo "== 1. check $policy against the live users and nodes (without its tests block)"
# The check reads the file inside the container, so it goes to /tmp there. Distroless image: no shell, so docker cp.
cid=$($compose ps -q "$svc")
docker cp "$tmp/new.json" "$cid:/tmp/policy-apply.json" >/dev/null || die "cannot copy the policy into $svc"
if ! hs policy check -f /tmp/policy-apply.json > "$tmp/check.log" 2>&1; then
  cat "$tmp/check.log" >&2
  die "the live headscale refuses this policy. Nothing changed. Fix the policy (for example a login that matches two users, #253) and run this again."
fi
echo "ok: the live headscale accepts it"
$check_only && exit 0

# In file mode `policy get` returns the file as the container sees it.
hs policy get > "$tmp/seen.hujson" 2>/dev/null || die "cannot read the policy file through $svc"
canon "$tmp/seen.hujson" 1 > "$tmp/seen.json" 2>/dev/null || : > "$tmp/seen.json"
if ! cmp -s "$tmp/seen.json" "$tmp/new.json"; then
  echo "== 2. the container sees an older $policy (a new file replaced the bind-mounted one): recreate $svc"
  $compose up -d --force-recreate "$svc" >/dev/null
  i=0; while [ $i -lt 30 ]; do hs users list >/dev/null 2>&1 && break; sleep 1; i=$((i + 1)); done
  hs users list >/dev/null 2>&1 || die "headscale did not come back after the recreate. Check: $compose logs $svc"
  hs policy get > "$tmp/seen.hujson" 2>/dev/null && canon "$tmp/seen.hujson" 1 > "$tmp/seen.json" &&
    cmp -s "$tmp/seen.json" "$tmp/new.json" || die "the container still does not see the new $policy"
  echo "ok: headscale runs $policy"
  exit 0
fi

echo "== 2. reload (SIGHUP)"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "== 3. confirm in the headscale log"
# headscale 0.29.7 sometimes logs the SIGHUP and does not reload: send it again, up to 3 times.
i=0; result=
while [ $i -lt 15 ]; do
  case $i in 0|5|10) $compose kill -s HUP "$svc" >/dev/null ;; esac
  log=$($compose logs --no-log-prefix --since "$since" "$svc" 2>&1)
  case "$log" in
    *"Policy reload completed"*) result=ok; break ;;
    *"reloading policy"*ERR*|*ERR*"reloading policy"*) result=failed; break ;;
  esac
  sleep 1; i=$((i + 1))
done
case "$result" in
  ok) ;;
  failed)
    err=$(printf '%s\n' "$log" | grep -i 'reloading policy')
    printf '%s\n' "$err" >&2
    case "$err" in
      *"test(s) failed"*)
        echo "only the tests block failed (its nodes are not on this tailnet); restarting $svc to load the policy"
        $compose restart "$svc" >/dev/null
        i=0; while [ $i -lt 30 ]; do hs users list >/dev/null 2>&1 && break; sleep 1; i=$((i + 1)); done
        hs users list >/dev/null 2>&1 || die "headscale did not come back after the restart. Check: $compose logs $svc"
        ;;
      *) die "headscale did not load the policy and keeps the old one. See the error above." ;;
    esac ;;
  *) die "no reload message from headscale in 15 s. Check: $compose logs $svc" ;;
esac
echo "ok: headscale runs $policy"

#!/usr/bin/env bash
# Tests for scripts/fly-snapshot.sh and the snapshot step of scripts/deploy-fly.sh (#197). A fake flyctl keeps the
# volumes and snapshots in files and records each deploy. No Fly, no network.
#   bash scripts/test/fly-snapshot/run_tests.sh
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
pass=0; fails=0
ok()  { echo "ok   $1"; pass=$((pass + 1)); }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }
command -v jq >/dev/null || { echo "SKIP: jq"; exit 0; }

# Fake flyctl. State: $FS/vols.json (volumes list), $FS/snaps (one "id status created_at" per line).
# FAKE_SNAP: ok (ready on the 2nd list), fail (create fails), stuck (never ready), bad (status failed).
cat > "$T/flyctl" <<'EOF'
#!/bin/sh
FS="$FAKE_FLY_STATE"
case "$1 $2 ${3:-}" in
  "volumes list "*) cat "$FS/vols.json" ;;
  "volumes snapshots list") n=$(cat "$FS/polls" 2>/dev/null || echo 0); echo $((n + 1)) > "$FS/polls"
    if [ -f "$FS/pending" ] && [ "$n" -ge 1 ]; then
      case "$FAKE_SNAP" in ok) sed -i.b 's/ creating / created /' "$FS/snaps" ;; bad) sed -i.b 's/ creating / failed /' "$FS/snaps" ;; esac
    fi
    awk 'BEGIN{printf "["} {printf "%s{\"id\":\"%s\",\"status\":\"%s\",\"created_at\":\"%s\"}", (NR>1?",":""), $1, $2, $3} END{print "]"}' "$FS/snaps" ;;
  "volumes snapshots create")
    [ "$FAKE_SNAP" = fail ] && { echo "Error: boom" >&2; exit 1; }
    echo "vs_new creating 2026-11-09T10:00:00Z" >> "$FS/snaps"; : > "$FS/pending"; echo "0" > "$FS/polls"
    echo "Scheduled to snapshot volume $4" ;;
  "deploy "*) echo "deployed $*" >> "$FS/deploys" ;;
  *) echo "fake flyctl: $*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/flyctl"
export FLYCTL="$T/flyctl" FAKE_FLY_STATE="$T/fs" SNAPSHOT_POLL=0 SNAPSHOT_TIMEOUT=3
reset() {
  rm -rf "$T/fs"; mkdir -p "$T/fs"
  echo '[{"id":"vol_1","name":"headscale_data","state":"created"}]' > "$T/fs/vols.json"
  echo "vs_old created 2026-11-01T03:00:00Z" > "$T/fs/snaps"
}
snap() { "$root/scripts/fly-snapshot.sh" "$@"; }

# fly-snapshot.sh
reset; out="$(FAKE_SNAP=ok snap my-hs 2>/dev/null)"; rc=$?
[ $rc = 0 ] && [ "$out" = vs_new ] && ok "a new snapshot: waits until it is created, prints its ID (not the old one)" || nok "ok case rc=$rc out=$out"
reset; FAKE_SNAP=fail snap my-hs >/dev/null 2>&1 && nok "create failure must fail" || ok "create fails: exit non-zero"
reset; out="$(FAKE_SNAP=stuck snap my-hs 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "no ready snapshot" && ok "never ready: times out and fails" || nok "stuck: $out"
reset; out="$(FAKE_SNAP=bad snap my-hs 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "failed" && ok "status failed: exit non-zero" || nok "bad: $out"
reset; echo '[{"id":"vol_1","name":"a"},{"id":"vol_2","name":"b"}]' > "$T/fs/vols.json"
out="$(FAKE_SNAP=ok snap my-hs 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "has 2 volumes" && ok "two volumes and no name: refused" || nok "two vols: $out"
out="$(FAKE_SNAP=ok snap my-hs b 2>/dev/null)"; [ "$out" = vs_new ] && ok "two volumes with a name: that volume" || nok "named vol: $out"

# deploy-fly.sh in a throwaway site repo
export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
core="$T/core"; mkdir -p "$core"
(cd "$root" && git ls-files -co --exclude-standard -z | while IFS= read -r -d '' f; do [ -f "$f" ] && printf '%s\0' "$f"; done \
  | tar --null -T - -cf -) | tar -xf - -C "$core"
git -C "$core" init -q && git -C "$core" add -A && git -C "$core" commit -qm core
site="$T/site"; mkdir -p "$site" && cd "$site" || exit 1
git init -q && git -c protocol.file.allow=always submodule -q add "$core" vendor/headlamp
mkdir -p fly/headscale fly/node-control && printf 'app = "my-hs"\n' > fly/headscale/fly.toml && echo '{}' > fly/node-control/nodes.json
git add -A && git commit -qm site
dep() { vendor/headlamp/scripts/deploy-fly.sh "$@"; }

reset; FAKE_SNAP=ok dep headscale >/dev/null 2>&1; rc=$?
[ $rc = 0 ] && grep -q "^deployed deploy" "$T/fs/deploys" && grep -q " created " "$T/fs/snaps" && [ "$(wc -l < "$T/fs/snaps" | tr -d " ")" = 2 ] \
  && ok "headscale deploy: a new snapshot first, then the deploy" || nok "headscale deploy rc=$rc"
reset; FAKE_SNAP=fail dep headscale >/dev/null 2>&1; rc=$?
[ $rc != 0 ] && [ ! -e "$T/fs/deploys" ] && ok "snapshot fails: no deploy" || nok "deployed without a snapshot (rc=$rc)"
[ -z "$(git -C vendor/headlamp status --porcelain)" ] && ok "submodule clean after a refused deploy" || nok "submodule dirty"
reset; FAKE_SNAP=fail NO_SNAPSHOT=1 dep headscale >/dev/null 2>&1; rc=$?
[ $rc = 0 ] && grep -q "^deployed" "$T/fs/deploys" && [ "$(wc -l < "$T/fs/snaps" | tr -d " ")" = 1 ] && ok "NO_SNAPSHOT=1: deploy with no snapshot" || nok "opt-out rc=$rc"
reset; FAKE_SNAP=fail dep node-control >/dev/null 2>&1; rc=$?
[ $rc = 0 ] && grep -q "^deployed" "$T/fs/deploys" && ok "node-control (not in SNAPSHOT_APPS): no snapshot needed" || nok "node-control rc=$rc"
reset; FAKE_SNAP=fail SNAPSHOT_APPS="node-control" dep node-control >/dev/null 2>&1 && nok "SNAPSHOT_APPS ignored" \
  || ok "SNAPSHOT_APPS=node-control: node-control needs a snapshot too"
out="$(dep --dry-run headscale 2>&1)"; printf '%s' "$out" | grep -q "first a volume snapshot" && ok "dry run says a snapshot comes first" || nok "dry run: $out"
printf 'primary_region = "x"\n' > fly/headscale/fly.toml
reset; out="$(FAKE_SNAP=ok dep headscale 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q 'no app = ' && [ ! -e "$T/fs/deploys" ] \
  && ok "no app name in fly.toml: refused, not deployed" || nok "no app name: $out"

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

#!/usr/bin/env bash
# Tests for apps/fly/exit-ams/state-sync.sh. No network: redis-cli, age and age-keygen are stubs on PATH.
# The stubs keep "Redis" in files under $tmp/redis/ and "encrypt" with a reversible transform tagged with the
# recipient. Env overrides used here: STATE_POLL_SECONDS=1 and BOOT_WAIT_SECONDS=2 (state-sync.sh waits that many
# seconds for Redis at boot; the default is 60). A real-age round trip runs only if age and age-keygen are installed.
set -u
here="$(cd "$(dirname "$0")" && pwd)"; root="$here/../../.."
sync="$root/apps/fly/exit-ams/state-sync.sh"
tmp="$(mktemp -d)"
bgpids=""
cleanup() { for p in $bgpids; do kill "$p" 2>/dev/null; done; rm -rf "$tmp"; }
trap cleanup EXIT
fails=0
ok()   { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

mkdir -p "$tmp/bin" "$tmp/redis"
cat > "$tmp/bin/redis-cli" <<'STUB'
#!/usr/bin/env bash
# args: -u URL --no-auth-warning [-x] CMD [key]
echo "$*" >> "$TESTTMP/redis_calls.log"
x=0
while [ $# -gt 0 ]; do
  case "$1" in -u) shift;; --no-auth-warning) ;; -x) x=1;; *) break;; esac
  shift
done
cmd="$1"; key="${2:-}"; f="$TESTTMP/redis/${key//:/_}"
[ -e "$TESTTMP/redis_down" ] && { echo "Could not connect to Redis" >&2; exit 1; }
case "$cmd" in
  PING) echo PONG;;
  EXISTS) if [ -f "$f" ]; then echo 1; else echo 0; fi;;
  GET) [ -f "$f" ] && cat "$f";;
  SET) [ "$x" = 1 ] || exit 2; cat > "$f"; echo "$key" >> "$TESTTMP/redis/set.log"; echo OK;;
  *) exit 2;;
esac
STUB
cat > "$tmp/bin/age-keygen" <<'STUB'
#!/usr/bin/env bash
[ "$1" = -y ] || exit 2
echo "pub-$(tr -d '\n' < "$2")"
STUB
cat > "$tmp/bin/age" <<'STUB'
#!/usr/bin/env bash
# encrypt: age -r RECIPIENT -a   decrypt: age -d -i IDFILE   (stdin -> stdout)
rot() { tr 'A-Za-z' 'N-ZA-Mn-za-m'; }
if [ "$1" = -r ]; then
  echo "ENC $2"; rot
elif [ "$1" = -d ] && [ "$2" = -i ]; then
  IFS= read -r hdr
  [ "$hdr" = "ENC pub-$(tr -d '\n' < "$3")" ] || { echo "age: no identity matched" >&2; exit 1; }
  rot
else exit 2; fi
STUB
chmod +x "$tmp/bin/"*
real_age="$(command -v age || true)"; real_keygen="$(command -v age-keygen || true)" # before the stubs shadow them
export TESTTMP="$tmp"
PATH="$tmp/bin:$PATH"
export REDIS_URL=redis://default:pw@redis.example:6379 STATE_AGE_IDENTITY=SECRET-A
export STATE_REDIS_KEY=app:state STATE_POLL_SECONDS=1 BOOT_WAIT_SECONDS=2

state="$tmp/state/tailscaled.state"; export STATE_FILE="$state"
rkey="$tmp/redis/app_state"; setlog="$tmp/redis/set.log"
# wait (max 20 s) until the background watch has printed "watching" into file $1
await_watch() { i=0; while ! grep -q "state-sync: watching" "$1" 2>/dev/null && [ "$i" -lt 40 ]; do sleep 0.5; i=$((i + 1)); done; }
# wait (max 15 s) until set.log has at least $1 lines
await_sets() { i=0; while [ "$(nsets)" -lt "$1" ] && [ "$i" -lt 30 ]; do sleep 0.5; i=$((i + 1)); done; }
reset() { rm -rf "$tmp/state" "$tmp/redis" "$tmp/redis_down" "$tmp/redis_calls.log"; mkdir -p "$tmp/state" "$tmp/redis"; }
# put an "encrypted" value into the fake Redis, as identity $1 would have produced it
seed_redis() { printf '%s' "$2" | PATH="$tmp/bin:$PATH" age -r "pub-$1" -a > "$rkey"; }
nsets() { if [ -f "$setlog" ]; then wc -l < "$setlog" | tr -d ' '; else echo 0; fi; }
doc='{"Profile":"x","_current-profile":"y"}'

reset; seed_redis SECRET-A "$doc"; echo old > "$state"
"$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$state")" = "$doc" ] && ok "restore: key present -> STATE_FILE replaced with decrypted content" || fail "restore present: rc=$rc content=$(cat "$state")"

reset; echo local-state > "$state"
"$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 0 ] && [ "$(nsets)" = 1 ] && [ "$(cat "$state")" = local-state ] && [ -f "$rkey" ] && ok "restore: key missing + file present -> Redis seeded, file untouched" || fail "restore seed: rc=$rc sets=$(nsets)"

reset
"$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 0 ] && [ ! -e "$state" ] && [ "$(nsets)" = 0 ] && ok "restore: key missing + no file -> exit 0, no file created" || fail "restore fresh: rc=$rc"

reset; echo local-state > "$state"; touch "$tmp/redis_down"
err="$("$sync" restore 2>&1 >/dev/null)"; rc=$?
[ "$rc" = 0 ] && [ "$(cat "$state")" = local-state ] && echo "$err" | grep -q "falling back" && ok "restore: redis down + file present -> exit 0, file untouched, stderr contains \"falling back\"" || fail "restore down+file: rc=$rc err=$err"

reset; touch "$tmp/redis_down"
"$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 1 ] && [ ! -e "$state" ] && ok "restore: redis down + no file -> exit 1, no file" || fail "restore down+nofile: rc=$rc"

reset; seed_redis SECRET-B "$doc"; echo local-state > "$state"; before="$(cksum < "$state")"
"$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 1 ] && [ "$(cksum < "$state")" = "$before" ] && [ "$(ls "$tmp/state" | wc -l | tr -d ' ')" = 1 ] && ok "restore: wrong identity -> exit 1, file byte-identical (cksum equal)" || fail "restore wrong id: rc=$rc"

reset
"$sync" push >/dev/null 2>&1; rc1=$?; : > "$state"; "$sync" push >/dev/null 2>&1; rc2=$?
[ "$rc1" = 0 ] && [ "$rc2" = 0 ] && [ "$(nsets)" = 0 ] && ok "push: empty or missing file -> no SET" || fail "push empty: $rc1 $rc2 sets=$(nsets)"

# watch: started with no file; file appears -> 1 push; unchanged -> still 1; changed -> 2
reset
"$sync" watch > "$tmp/watch.out" 2>&1 & wpid=$!; bgpids="$bgpids $wpid"
await_watch "$tmp/watch.out"; echo v1 > "$state"; await_sets 1
n1="$(nsets)"; sleep 3; n2="$(nsets)"
echo v2 > "$state"; await_sets 2; n3="$(nsets)"
kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
[ "$n1" = 1 ] && [ "$n2" = 1 ] && [ "$n3" = 2 ] && grep -q "state-sync: pushed" "$tmp/watch.out" && ok "watch: unchanged file over 3 s -> set.log has exactly 1 line; changed file -> 2 lines" || fail "watch: $n1 $n2 $n3"

# watch must not push a file that existed at start (restore seeds the checksum)
reset; echo v1 > "$state"
"$sync" watch > "$tmp/watch2.out" 2>&1 & wpid=$!; bgpids="$bgpids $wpid"
await_watch "$tmp/watch2.out"; sleep 3; n="$(nsets)"; kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
[ "$n" = 0 ] && ok "watch: file present at start and unchanged -> no push" || fail "watch initial push: $n"

# watch retries a failed SET on the next cycle
reset
"$sync" watch > "$tmp/watch3.out" 2>&1 & wpid=$!; bgpids="$bgpids $wpid"
await_watch "$tmp/watch3.out"; touch "$tmp/redis_down"; echo v1 > "$state"; sleep 3; a="$(nsets)"
rm -f "$tmp/redis_down"; await_sets 1; b="$(nsets)"
kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
[ "$a" = 0 ] && [ "$b" = 1 ] && ok "watch: failed push is retried on a later cycle" || fail "watch retry: $a $b"

reset; echo x > "$state"; rm -f "$tmp/redis_calls.log"
rcs=""
for sub in restore push; do env -u REDIS_URL "$sync" "$sub" >/dev/null 2>&1; rcs="$rcs $?"; done
env -u REDIS_URL "$sync" watch >/dev/null 2>&1 & wpid=$!; bgpids="$bgpids $wpid"
sleep 2; if kill -0 "$wpid" 2>/dev/null; then alive=1; else alive=0; fi
kill "$wpid" 2>/dev/null; wait "$wpid" 2>/dev/null
env -u REDIS_URL REDIS_URL= "$sync" restore >/dev/null 2>&1; rcs="$rcs $?"
[ "$rcs" = " 0 0 0" ] && [ "$alive" = 0 ] && [ ! -e "$tmp/redis_calls.log" ] && ok "REDIS_URL unset: restore/watch/push exit 0 and run no redis-cli" || fail "disabled: rcs=$rcs alive=$alive calls=$(cat "$tmp/redis_calls.log" 2>/dev/null)"

"$sync" bogus >/dev/null 2>&1; r1=$?; "$sync" >/dev/null 2>&1; r2=$?
env -u REDIS_URL "$sync" bogus >/dev/null 2>&1; r3=$?
[ "$r1" = 64 ] && [ "$r2" = 64 ] && [ "$r3" = 64 ] && ok "bad subcommand -> exit 64" || fail "usage: $r1 $r2 $r3"

# missing identity while enabled is fatal
reset; echo x > "$state"
env -u STATE_AGE_IDENTITY "$sync" restore >/dev/null 2>&1; rc=$?
[ "$rc" = 1 ] && ok "REDIS_URL set but no STATE_AGE_IDENTITY -> exit 1" || fail "missing identity: rc=$rc"

# real age round trip (push, then restore on a clean file) with a fake redis
if [ -n "$real_age" ] && [ -n "$real_keygen" ]; then
  real="$(mktemp -d)"; realpath_="$real_age"; realkeygen="$real_keygen"
  mkdir -p "$real/bin"; ln -s "$realpath_" "$real/bin/age"; ln -s "$realkeygen" "$real/bin/age-keygen"
  cp "$tmp/bin/redis-cli" "$real/bin/redis-cli"
  ident="$("$realkeygen" 2>/dev/null | grep '^AGE-SECRET-KEY-')"
  reset; printf '%s\n' "$doc" > "$state"
  PATH="$real/bin:/usr/bin:/bin" STATE_AGE_IDENTITY="$ident" "$sync" push >/dev/null 2>&1; rc1=$?
  rm -f "$state"
  PATH="$real/bin:/usr/bin:/bin" STATE_AGE_IDENTITY="$ident" "$sync" restore >/dev/null 2>&1; rc2=$?
  [ "$rc1" = 0 ] && [ "$rc2" = 0 ] && [ "$(cat "$state")" = "$doc" ] && ! grep -q "$doc" "$rkey" && ok "real age: push then restore round trip (value encrypted in Redis)" || fail "real age: $rc1 $rc2"
  rm -rf "$real"
else
  echo "skip - real age round trip (age/age-keygen not installed)"
fi
exit "$fails"

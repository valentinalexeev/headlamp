#!/bin/sh
# Tests for scripts/headscale-policy.sh. No network: curl and flyctl are fakes on PATH; jq and git are real.
#   sh scripts/test/headscale-policy/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../headscale-policy.sh"
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq is not installed; the policy client tests need it"
  exit 0
fi
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
BIN="$T/bin"; FIX="$T/fix"; CALLS="$T/calls"; FLY="$T/flyctl.args"
mkdir -p "$BIN"

# Fake curl: answers from $FIX/<METHOD>_<path with / as _>[.<nth call>].{status,body,fail}; logs "METHOD /path".
cat > "$BIN/curl" <<'STUB'
#!/bin/sh
method=GET; out=; body=; url=
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method=$2; shift ;;
    -o) out=$2; shift ;;
    -w|-H|--max-time) shift ;;
    --data-binary) body=${2#@}; shift ;;
    http*) url=$1 ;;
  esac
  shift
done
path=${url#*://}; path=/${path#*/}
key=${method}_$(printf '%s' "${path#/}" | tr / _)
n=0; [ -f "$FIX/count.$key" ] && n=$(cat "$FIX/count.$key"); n=$((n + 1)); echo "$n" > "$FIX/count.$key"
echo "$method $path" >> "$CALLS"
[ -n "$body" ] && cp "$body" "$FIX/lastbody.$key"
for f in "$key.$n" "$key"; do
  if [ -e "$FIX/$f.fail" ]; then echo "curl: (7) failed to connect" >&2; exit 7; fi
  if [ -e "$FIX/$f.status" ]; then
    [ -n "$out" ] && cp "$FIX/$f.body" "$out"
    printf '%s' "$(cat "$FIX/$f.status")"
    exit 0
  fi
done
echo "curl: (7) no fixture for $key" >&2; exit 7
STUB
cat > "$BIN/flyctl" <<'STUB'
#!/bin/sh
for last; do :; done
case $last in
  *"policy get"*)
    echo get >> "$FLY_CALLS"
    [ -e "$FIX/flyctl.getfail" ] && exit 1
    [ -f "$FIX/flyctl.get.out" ] && cat "$FIX/flyctl.get.out"
    exit 0 ;;
esac
echo set >> "$FLY_CALLS"
for a in "$@"; do printf '%s\n' "$a"; done > "$FLY_ARGS"
[ -e "$FIX/flyctl.fail" ] && exit 1
exit 0
STUB
chmod +x "$BIN/curl" "$BIN/flyctl"

export FIX CALLS PATH="$BIN:$PATH" FLY_ARGS="$FLY" FLY_CALLS="$T/flycalls" RETRY_SLEEP=0
export NODE_CONTROL_URL="http://nc.example.test/"
unset POLICY_FILE HS_APP

pass=0; fails=0
ok()   { echo "ok   - $1"; pass=$((pass + 1)); }
bad()  { echo "FAIL - $1"; fails=$((fails + 1)); }
expect() { # description condition-exit-status
  if [ "$2" = 0 ]; then ok "$1"; else bad "$1"; fi
}
fx() { # KEY STATUS BODY [N]
  base="$FIX/$1${4:+.$4}"
  printf '%s' "$2" > "$base.status"; printf '%s' "$3" > "$base.body"
}
fxfail() { : > "$FIX/$1${2:+.$2}.fail"; }
reset() { rm -rf "$FIX"; mkdir -p "$FIX"; : > "$CALLS"; rm -f "$FLY" "$T/flycalls"; }
run() { sh "$script" "$@" > "$T/out" 2> "$T/err"; rc=$?; }
ncalls() { wc -l < "$CALLS" | tr -d ' '; }
order() { tr '\n' ',' < "$CALLS"; }
has() { grep -q -- "$1" "$2"; }

POL='{"acls":[{"action":"accept","src":["alice"],"dst":["*:*"]}]}'
OK_PREVIEW='{"valid":true,"error":"","diff":"--- live\n+++ new\n+x\n","warnings":[]}'
state_json() { jq -n --arg p "$1" --arg u "2026-10-24T10:00:00Z" '{policy:$p,updated_at:$u,pending:null,history:[]}'; }
FILE="$T/acl.json"
printf '%s' "$POL" > "$FILE"

# --- check
reset; fx POST_policy_preview 200 "$OK_PREVIEW"
run check --file "$FILE"
expect "check valid exits 0" "$([ $rc = 0 ]; echo $?)"
[ "$(order)" = "POST /policy/preview," ] && ok "check posts to /policy/preview once (trailing slash stripped)" || bad "check calls: $(order)"
jq -e --arg p "$POL" '.policy == $p' "$FIX/lastbody.POST_policy_preview" >/dev/null && ok "check sends the file as {policy}" || bad "check body"

reset; fx POST_policy_preview 200 '{"valid":true,"error":"","diff":"","warnings":["no acls","tag:x unknown"]}'
run check --file "$FILE"
{ [ $rc = 0 ] && has 'warning: no acls' "$T/err" && has 'warning: tag:x unknown' "$T/err"; } && ok "check prints warnings" || bad "check warnings rc=$rc"

reset; fx POST_policy_preview 200 '{"valid":false,"error":"bad rule at line 3","diff":"","warnings":[]}'
run check --file "$FILE"
{ [ $rc = 2 ] && has 'bad rule at line 3' "$T/err" && [ "$(order)" = "POST /policy/preview," ]; } && ok "check invalid exits 2 with the server error" || bad "check invalid rc=$rc"

# --- diff
reset; fx GET_policy_state 200 "$(state_json "$POL")"
run diff --file "$FILE"
{ [ $rc = 0 ] && [ ! -s "$T/out" ]; } && ok "diff equal exits 0" || bad "diff equal rc=$rc"

reset; fx GET_policy_state 200 "$(state_json '{
  "acls": [ {"src": ["alice"], "dst": ["*:*"], "action": "accept"} ]
}')"
run diff --file "$FILE"
[ $rc = 0 ] && ok "diff ignores formatting and key order" || bad "diff formatting rc=$rc"

reset; fx GET_policy_state 200 "$(state_json '{"acls":[]}')"
run diff --file "$FILE"
{ [ $rc = 1 ] && has '^--- live' "$T/out" && has '^+++ file' "$T/out" && has 'alice' "$T/out"; } && ok "diff different exits 1 with a unified diff on stdout" || bad "diff different rc=$rc"

reset; printf 'plain text\n' > "$T/plain"; fx GET_policy_state 200 "$(state_json 'plain text
')"
run diff --file "$T/plain"
[ $rc = 0 ] && ok "diff compares raw text when not JSON" || bad "diff raw rc=$rc"

# --- apply: git and confirmation guards
# apply_ok: fixtures for a full successful run (state is read twice: base, then the verification).
apply_ok() {
  reset; fx POST_policy_preview 200 "$OK_PREVIEW"
  fx GET_policy_state 200 "$(state_json '{"acls":[]}')" 1
  fx GET_policy_state 200 "$(state_json "$POL")" 2
  fx POST_policy_apply 200 '{"change_id":7,"deadline":"2026-10-24T10:02:00Z"}'
  fx POST_policy_confirm 204 ''
}
G="$T/repo"; mkdir -p "$G/sub"
git -C "$G" init -q
printf '%s' "$POL" > "$G/sub/acl.json"
git -C "$G" add sub/acl.json
git -C "$G" -c user.name=t -c user.email=t@example.test -c commit.gpgsign=false commit -q -m init
printf '%s' '{"acls":[]}' > "$G/sub/acl.json"
reset
run apply --yes --file "$G/sub/acl.json"
{ [ $rc = 2 ] && [ "$(ncalls)" = 0 ] && has 'uncommitted' "$T/err"; } && ok "apply on a dirty file exits 2 without network" || bad "apply dirty rc=$rc calls=$(ncalls)"

reset
( cd "$G" && sh "$script" apply --yes --file sub/acl.json > "$T/out" 2> "$T/err" ); rc=$?
{ [ $rc = 2 ] && [ "$(ncalls)" = 0 ]; } && ok "dirty guard works with a relative path that has a directory part" || bad "relative dirty rc=$rc calls=$(ncalls)"

apply_ok; fx GET_policy_state 200 "$(state_json '{"acls":[]}')" 2
run apply --yes --allow-dirty --file "$G/sub/acl.json"
[ $rc = 0 ] && ok "apply --allow-dirty skips the git guard" || bad "allow-dirty rc=$rc"
git -C "$G" checkout -q sub/acl.json

apply_ok
( cd "$G" && sh "$script" apply --yes --file sub/acl.json > "$T/out" 2> "$T/err" ); rc=$?
[ $rc = 0 ] && ok "a committed file with a relative path proceeds" || bad "relative clean rc=$rc: $(cat "$T/err")"

reset; fx POST_policy_preview 200 "$OK_PREVIEW"; fx GET_policy_state 200 "$(state_json '{}')"
run apply --file "$FILE" < /dev/null
{ [ $rc = 2 ] && has 'refusing to apply without --yes' "$T/err" && ! has 'POST /policy/apply' "$CALLS"; } && ok "apply without --yes non-interactively exits 2" || bad "apply no-yes rc=$rc"

# --- apply happy path
apply_ok
run apply --yes --file "$FILE"
[ $rc = 0 ] && ok "apply --yes exits 0" || bad "apply happy rc=$rc: $(cat "$T/err")"
[ "$(order)" = "GET /policy/state,POST /policy/preview,POST /policy/apply,GET /policy/state,POST /policy/confirm," ] && ok "apply call order: state, preview, apply, state, confirm" || bad "apply order: $(order)"
jq -e --arg p "$POL" '.policy == $p and .base_updated_at == "2026-10-24T10:00:00Z"' "$FIX/lastbody.POST_policy_apply" >/dev/null && ok "apply sends policy and base_updated_at" || bad "apply body: $(cat "$FIX/lastbody.POST_policy_apply")"
jq -e '. == {change_id: 7}' "$FIX/lastbody.POST_policy_confirm" >/dev/null && ok "confirm body carries the change id of the apply" || bad "confirm body: $(cat "$FIX/lastbody.POST_policy_confirm")"
has '+x' "$T/out" && ok "apply prints the preview diff" || bad "apply diff output"

apply_ok
fx GET_policy_state 200 "$(state_json '{
  "acls": [ {"src": ["alice"], "dst": ["*:*"], "action": "accept"} ]
}')" 2
run apply --yes --file "$FILE"
[ $rc = 0 ] && ok "apply accepts a reformatted but equal live policy" || bad "apply reformat rc=$rc"

# --- apply error paths
apply_ok; fx GET_policy_state 200 "$(state_json '{"acls":[]}' )" 2
run apply --yes --file "$FILE"
{ [ $rc = 4 ] && ! has 'confirm' "$CALLS" && has 'live policy is not the one that was applied' "$T/err"; } && ok "apply: live policy differs after apply -> no confirm, exit 4" || bad "apply mismatch rc=$rc"

apply_ok; fx POST_policy_apply 202 '{"change_id":3,"uncertain":true,"detail":"outcome unknown; an automatic revert is armed"}'
fx GET_policy_state 200 "$(state_json '{"acls":[]}')" 2
run apply --yes --file "$FILE"
{ [ $rc = 4 ] && ! has 'confirm' "$CALLS" && has 'warning:' "$T/err"; } && ok "apply 202 uncertain with a different live policy -> exit 4, no confirm" || bad "apply 202 mismatch rc=$rc"

apply_ok; fx POST_policy_confirm 409 '{"detail":"no change is pending"}'
run apply --yes --file "$FILE"
{ [ $rc = 4 ] && has 'not confirmed' "$T/err"; } && ok "apply: confirm 409 exits 4" || bad "apply confirm 409 rc=$rc"

apply_ok; fx POST_policy_apply 409 '{"detail":"the policy changed since you loaded it"}'
fx GET_policy_state 200 "$(state_json '{}')" 1
run apply --yes --file "$FILE"
{ [ $rc = 2 ] && ! has 'confirm' "$CALLS" && has 'another change is pending' "$T/err"; } && ok "apply 409 (state moved after the read) exits 2 without confirm" || bad "apply 409 rc=$rc"

apply_ok; fx POST_policy_apply 422 '{"detail":"headscale says no: bad tag"}'
run apply --yes --file "$FILE"
{ [ $rc = 2 ] && has 'headscale says no: bad tag' "$T/err" && ! has 'confirm' "$CALLS"; } && ok "apply 422 exits 2 and prints the detail" || bad "apply 422 rc=$rc"

apply_ok; fx POST_policy_apply 413 '{"detail":"the policy is too large"}'
run apply --yes --file "$FILE"
{ [ $rc = 2 ] && has 'too large' "$T/err"; } && ok "apply 413 exits 2" || bad "apply 413 rc=$rc"

apply_ok; fx POST_policy_apply 202 '{"change_id":3,"uncertain":true,"detail":"outcome unknown; an automatic revert is armed"}'
run apply --yes --file "$FILE"
{ [ $rc = 0 ] && has 'warning:' "$T/err" && has 'confirm' "$CALLS"; } && ok "apply 202 uncertain warns and continues" || bad "apply 202 rc=$rc"
jq -e '. == {change_id: 3}' "$FIX/lastbody.POST_policy_confirm" >/dev/null && ok "202 confirm uses the id from the 202 answer" || bad "202 confirm body"

apply_ok; fx POST_policy_apply 500 '{}'
run apply --yes --file "$FILE"
[ $rc = 3 ] && ok "apply 500 exits 3" || bad "apply 500 rc=$rc"

apply_ok; fx POST_policy_apply 200 '{"deadline":"x"}'
run apply --yes --file "$FILE"
{ [ $rc = 3 ] && ! has 'confirm' "$CALLS"; } && ok "apply answer without change_id exits 3 without confirm" || bad "no change_id rc=$rc"

apply_ok; rm -f "$FIX/GET_policy_state.2."*
fxfail GET_policy_state 2; fxfail GET_policy_state 3; fxfail GET_policy_state 4
run apply --yes --file "$FILE"
{ [ $rc = 4 ] && ! has 'confirm' "$CALLS" && has 'reverts automatically' "$T/err"; } && ok "apply: lost access after the change -> no confirm, exit 4" || bad "apply lockout rc=$rc"
[ "$(grep -c 'GET /policy/state' "$CALLS")" = 4 ] && ok "apply retries the verification 3 times" || bad "verify retries: $(order)"

apply_ok; fx POST_policy_confirm 500 '{}'
run apply --yes --file "$FILE"
{ [ $rc = 3 ] && has 'revert' "$T/err"; } && ok "apply: confirm failing exits 3 and says it will revert" || bad "apply confirm 500 rc=$rc"

# --- pull
reset; fx GET_policy_state 200 "$(state_json '{"acls":[]}
')"
printf 'old' > "$T/pulled.json"
run pull --file "$T/pulled.json"
{ [ $rc = 0 ] && [ "$(cat "$T/pulled.json"; echo x)" = '{"acls":[]}
x' ]; } && ok "pull writes the exact bytes" || bad "pull rc=$rc content=$(cat "$T/pulled.json")"
[ -z "$(ls -A "$T" | grep -F '.pulled.json')" ] && ok "pull leaves no temp file" || bad "pull temp file left"

reset; fx GET_policy_state 200 "$(state_json '{"acls":[]}')"
printf 'old' > "$T/mode.json"; chmod 640 "$T/mode.json"
run pull --file "$T/mode.json"
[ "$(ls -l "$T/mode.json" | cut -c1-10)" = "-rw-r-----" ] && ok "pull keeps the mode of an existing file" || bad "pull mode: $(ls -l "$T/mode.json")"
rm -f "$T/new.json"
run pull --file "$T/new.json"
[ "$(ls -l "$T/new.json" | cut -c1-10)" = "-rw-r--r--" ] && ok "pull creates a new file with mode 0644" || bad "pull new mode: $(ls -l "$T/new.json")"

reset; fxfail GET_policy_state
printf 'old' > "$T/pulled.json"
run pull --file "$T/pulled.json"
{ [ $rc = 3 ] && [ "$(cat "$T/pulled.json")" = old ]; } && ok "pull keeps the old file on a remote failure" || bad "pull failure rc=$rc"

reset; fx GET_policy_state 500 '{}'
run pull --file "$T/pulled.json"
{ [ $rc = 3 ] && [ "$(cat "$T/pulled.json")" = old ]; } && ok "pull keeps the old file on HTTP 500" || bad "pull 500 rc=$rc"

# --- bootstrap
reset
run bootstrap --yes --file "$FILE"
[ $rc = 2 ] && ok "bootstrap without HS_APP exits 2" || bad "bootstrap no app rc=$rc"

reset
HS_APP=hs-app run bootstrap --file "$FILE"
{ [ $rc = 2 ] && [ ! -e "$FLY" ]; } && ok "bootstrap without --yes exits 2" || bad "bootstrap no-yes rc=$rc"

reset
export HS_APP=hs-app
cp "$FILE" "$FIX/flyctl.get.out"
run bootstrap --yes --file "$FILE"
b64=$(base64 < "$FILE" | tr -d '\n')
{ [ $rc = 0 ] && has 'ssh' "$FLY" && has 'hs-app' "$FLY" && has "$b64" "$FLY" && has 'headscale policy set' "$FLY"; } && ok "bootstrap runs flyctl ssh with the base64 file" || bad "bootstrap rc=$rc args=$(cat "$FLY" 2>/dev/null)"
[ "$(ncalls)" = 0 ] && ok "bootstrap never contacts node-control" || bad "bootstrap used curl"
[ "$(tr '\n' ' ' < "$T/flycalls")" = "set get " ] && ok "bootstrap reads the policy back after setting it" || bad "bootstrap calls: $(cat "$T/flycalls")"

reset; printf '%s' '{ "acls": [] }' > "$FIX/flyctl.get.out"
run bootstrap --yes --file "$FILE"
{ [ $rc = 3 ] && has 'not loaded' "$T/err"; } && ok "bootstrap: read-back differs -> exit 3" || bad "bootstrap mismatch rc=$rc"

reset; printf '%s' "$POL" > "$FIX/flyctl.get.out"; : > "$FIX/flyctl.getfail"
run bootstrap --yes --file "$FILE"
[ $rc = 3 ] && ok "bootstrap: read-back failing -> exit 3" || bad "bootstrap get fail rc=$rc"

reset; fxfail flyctl
run bootstrap --yes --file "$FILE"
[ $rc = 3 ] && ok "bootstrap flyctl failure exits 3" || bad "bootstrap fail rc=$rc"

reset
head -c 70000 /dev/zero | tr '\0' 'a' > "$T/big.json"
run bootstrap --yes --file "$T/big.json"
{ [ $rc = 2 ] && [ ! -e "$FLY" ]; } && ok "bootstrap refuses a file over 64 KiB" || bad "bootstrap big rc=$rc"

HS_APP=-oProxyCommand=x run bootstrap --yes --file "$FILE"
[ $rc = 2 ] && ok "option-like app name exits 2" || bad "dash app rc=$rc"
unset HS_APP

# --- usage and transport errors
run frobnicate
[ $rc = 2 ] && ok "unknown subcommand exits 2" || bad "unknown subcommand rc=$rc"
run
[ $rc = 2 ] && ok "no subcommand exits 2" || bad "no subcommand rc=$rc"
(unset NODE_CONTROL_URL; sh "$script" check --file "$FILE" >/dev/null 2>&1); [ $? = 2 ] && ok "missing NODE_CONTROL_URL exits 2" || bad "missing url"
(NODE_CONTROL_URL=--evil sh "$script" check --file "$FILE" >/dev/null 2>&1); [ $? = 2 ] && ok "option-like NODE_CONTROL_URL exits 2" || bad "dash url"
run check --file "$T/missing.json"
[ $rc = 2 ] && ok "missing policy file exits 2" || bad "missing file rc=$rc"
run check --bogus
[ $rc = 2 ] && ok "unknown option exits 2" || bad "unknown option rc=$rc"

reset; fxfail POST_policy_preview
run check --file "$FILE"
[ $rc = 3 ] && ok "curl transport failure exits 3" || bad "transport rc=$rc"
reset; fx POST_policy_preview 403 ''
run check --file "$FILE"
[ $rc = 3 ] && ok "HTTP 403 exits 3" || bad "403 rc=$rc"
reset; fx GET_policy_state 503 ''
run diff --file "$FILE"
[ $rc = 3 ] && ok "diff HTTP 503 exits 3" || bad "diff 503 rc=$rc"

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

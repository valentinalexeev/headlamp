#!/bin/sh
# Tests for scripts/headscale-tag-node.sh. No network: curl is a fake on PATH (a tiny stateful headscale); jq is real.
#   sh scripts/test/headscale-tag-node/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../headscale-tag-node.sh"
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq is not installed; the tag helper tests need it"
  exit 0
fi
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
BIN="$T/bin"; FIX="$T/fix"; CALLS="$T/calls"; ARGV="$T/argv"; LEAK="$T/leak"; FIRSTARG="$T/firstarg"; SCRATCH="$T/tmp"
KEYDIR="$T/keys"   # where HEADSCALE_API_KEY_CMD reads its key: outside every scanned place
mkdir -p "$BIN" "$SCRATCH" "$KEYDIR"

CANARY='hskey-api-canary-s3cr3t'
CANARY2='hskey-api-canary-fr0m-cmd'
printf '%s' "$CANARY2" > "$KEYDIR/key"

# Fake curl: a small headscale. State: $FIX/nodes.json (the "nodes" array as headscale returns it). A node with a
# forcedTags field is a legacy node (SetTags replaces forcedTags); one with only `tags` is a unified node (0.28+,
# SetTags replaces tags).
# Flags: $FIX/unreachable (transport failure), $FIX/status (forced HTTP status for every call), $FIX/noop (POST answers
# 200 but changes nothing), $FIX/snake (answer with snake_case field names). Every call logs "METHOD /path", its argv
# and its first argument, checks the bearer key read from the -K - config on stdin against $EXPECT_KEY, and scans
# $TMPDIR for the canaries (a key on disk during the run is a leak).
cat > "$BIN/curl" <<'STUB'
#!/bin/sh
printf '%s\n' "$@" >> "$ARGV"
printf '%s\n' "$1" >> "$FIRSTARG"
method=GET; out=; body=; url=; cfg=0
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method=$2; shift ;;
    -o) out=$2; shift ;;
    -K) [ "$2" = - ] && cfg=1; shift ;;
    -w|-H|--max-time|--connect-timeout) shift ;;
    --data-binary) body=${2#@}; shift ;;
    http*) url=$1 ;;
  esac
  shift
done
path=${url#*://}; path=/${path#*/}
echo "$method $path" >> "$CALLS"
if grep -rq -e "$CANARY" -e "$CANARY2" "$TMPDIR" 2>/dev/null; then echo "curl $method $path" >> "$LEAK"; fi
got=
[ "$cfg" = 1 ] && got=$(sed -n 's/^header = "Authorization: Bearer \(.*\)"$/\1/p')
reply() { [ -n "$out" ] && printf '%s' "$2" > "$out"; printf '%s' "$1"; exit 0; }
[ -e "$FIX/unreachable" ] && { echo "curl: (7) failed to connect" >&2; exit 7; }
[ -e "$FIX/status" ] && reply "$(cat "$FIX/status")" '{"message":"denied"}'
[ "$got" = "$EXPECT_KEY" ] || reply 401 '{"message":"unauthorized"}'
case "$method $path" in
  "GET /api/v1/node")
    if [ -e "$FIX/snake" ]; then
      reply 200 "$(jq -c '{nodes: [.[] | {id, name, given_name: .givenName, forced_tags: .forcedTags, valid_tags: .validTags, invalid_tags: .invalidTags}]}' "$FIX/nodes.json")"
    fi
    reply 200 "$(jq -c '{nodes: .}' "$FIX/nodes.json")" ;;
  "POST /api/v1/node/"*"/tags")
    id=${path#/api/v1/node/}; id=${id%/tags}
    cp "$body" "$FIX/lastbody"
    if [ ! -e "$FIX/noop" ]; then
      # legacy nodes (with forcedTags) get their forced set replaced; unified nodes their tags
      jq --arg id "$id" --slurpfile b "$body" 'map(if (.id | tostring) == $id then
          (if has("forcedTags") then .forcedTags = $b[0].tags else .tags = $b[0].tags end) else . end)' \
        "$FIX/nodes.json" > "$FIX/nodes.new" && mv "$FIX/nodes.new" "$FIX/nodes.json"
    fi
    reply 200 "$(jq -c --arg id "$id" '{node: (.[] | select((.id | tostring) == $id))}' "$FIX/nodes.json")" ;;
esac
reply 404 '{"message":"not found"}'
STUB
chmod +x "$BIN/curl"

export FIX CALLS ARGV FIRSTARG LEAK CANARY CANARY2 TMPDIR="$SCRATCH" PATH="$BIN:$PATH"
export HEADSCALE_URL="https://headscale.example.com/" HEADSCALE_API_KEY="$CANARY" EXPECT_KEY="$CANARY"
unset HEADSCALE_API_KEY_CMD REQ_MAX_TIME

pass=0; fails=0
ok()  { echo "ok   - $1"; pass=$((pass + 1)); }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1 (rc=$rc; out: $(tr '\n' '|' < "$T/out"); err: $(tr '\n' '|' < "$T/err"))"; fi; }

NODES='[
 {"id":"1","name":"host-a","givenName":"runner-1","forcedTags":["tag:exit","tag:aaa"],"validTags":["tag:self"],"invalidTags":["tag:nope"],"tags":[]},
 {"id":"2","name":"host-b","givenName":"twin","forcedTags":[],"validTags":[]},
 {"id":"3","name":"host-c","givenName":"twin","forcedTags":[],"validTags":[]},
 {"id":"4","name":"runner-2","givenName":"runner-2-x","forcedTags":["tag:task-runner"],"validTags":[]}
]'
reset() { rm -rf "$FIX"; mkdir -p "$FIX"; printf '%s' "$NODES" > "$FIX/nodes.json"; : > "$CALLS"; : > "$ARGV"; : > "$LEAK"; : > "$FIRSTARG"; }
run() { sh "$script" "$@" > "$T/out" 2> "$T/err"; rc=$?; }
# runenv VAR=value... -- ARGS: like run with a changed environment (a prefix assignment on a shell function would stick)
# shellcheck disable=SC2163 # export "VAR=value" is intended
runenv() {
  ( while [ "$1" != -- ]; do export "$1"; shift; done; shift; exec sh "$script" "$@" ) > "$T/out" 2> "$T/err"; rc=$?
}
forced() { jq -c --arg id "$1" '.[] | select((.id | tostring) == $id) | .forcedTags' "$FIX/nodes.json"; }
utags() { jq -c --arg id "$1" '.[] | select((.id | tostring) == $id) | .tags' "$FIX/nodes.json"; }
posts() { grep -c '^POST' "$CALLS" | tr -d ' '; }
leak_check() { # DESCRIPTION: the keys must not be in stdout, stderr, any curl argv, the call log or a temp file
  _l=0
  for f in "$T/out" "$T/err" "$ARGV" "$CALLS"; do grep -q -e "$CANARY" -e "$CANARY2" "$f" && _l=1; done
  [ -s "$LEAK" ] && _l=1
  grep -rq -e "$CANARY" -e "$CANARY2" "$SCRATCH" 2>/dev/null && _l=1
  if [ "$_l" = 0 ]; then ok "$1"; else bad "$1"; fi
}

# --- show
reset; run show --node runner-1
check "show exits 0 and only reads" '[ $rc = 0 ] && [ "$(cat "$CALLS")" = "GET /api/v1/node" ]'
check "show prints id, the legacy model and the forced/valid/other split" 'grep -qx "node: runner-1 (id 1)" "$T/out" && grep -q "^model: legacy" "$T/out" && grep -qx "forced (admin-set): \[tag:aaa tag:exit\]" "$T/out" && grep -q "^valid .*: \[tag:self\]$" "$T/out" && grep -qx "other: \[\]" "$T/out"'
check "invalid tags are ignored" '! grep -q "tag:nope" "$T/out"'
check "show matches the given name, never the host name" 'run show --node host-a; [ $rc = 2 ]'
leak_check "show: the key is nowhere in output, argv or temp files"

reset; : > "$FIX/snake"; run show --node runner-1
check "snake_case field names are read too" '[ $rc = 0 ] && grep -qx "forced (admin-set): \[tag:aaa tag:exit\]" "$T/out" && grep -q "^valid .*: \[tag:self\]$" "$T/out"'

# --- add
reset; run add --node runner-1 --tag tag:task-runner
check "add without --yes: plan, exit 1, no POST" '[ $rc = 1 ] && [ "$(posts)" = 0 ] && grep -qx "forced: \[tag:aaa tag:exit\] -> \[tag:aaa tag:exit tag:task-runner\]" "$T/out" && grep -q "REPLACES" "$T/out"'
check "add without --yes leaves the node alone" '[ "$(forced 1)" = "[\"tag:exit\",\"tag:aaa\"]" ]'

reset; run add --node runner-1 --tag tag:task-runner --yes
check "add --yes exits 0" '[ $rc = 0 ] && grep -q "^applied" "$T/out"'
check "add keeps the other forced tags (sorted union sent)" '[ "$(jq -c . "$FIX/lastbody")" = "{\"tags\":[\"tag:aaa\",\"tag:exit\",\"tag:task-runner\"]}" ]'
check "add posts to /api/v1/node/1/tags (trailing slash of the URL stripped)" 'grep -qx "POST /api/v1/node/1/tags" "$CALLS" && ! grep -q "//api" "$CALLS"'
check "add re-reads the node after the POST" '[ "$(tr "\n" , < "$CALLS")" = "GET /api/v1/node,POST /api/v1/node/1/tags,GET /api/v1/node," ]'
check "valid tags are never sent as forced" '! grep -q "tag:self" "$FIX/lastbody"'
leak_check "add: the key is nowhere in output, argv or temp files"
check "every curl call starts with -q" '[ -s "$FIRSTARG" ] && [ "$(sort -u "$FIRSTARG")" = "-q" ]'
check "curl reads its config (the key) from stdin" 'grep -qx -- "-K" "$ARGV"'

reset; run add --node runner-2-x --tag tag:task-runner --yes
check "add is idempotent: no POST when the tag is already forced" '[ $rc = 0 ] && [ "$(posts)" = 0 ] && grep -q "(no change)" "$T/out"'

# --- remove
reset; run remove --node runner-1 --tag tag:exit --yes
check "remove keeps the rest" '[ $rc = 0 ] && [ "$(jq -c . "$FIX/lastbody")" = "{\"tags\":[\"tag:aaa\"]}" ] && [ "$(forced 1)" = "[\"tag:aaa\"]" ]'
reset; run remove --node runner-1 --tag tag:absent --yes
check "remove of an absent tag changes nothing" '[ $rc = 0 ] && [ "$(posts)" = 0 ]'
reset; run remove --node runner-1 --tag tag:self --yes
check "remove of a requested-only tag notes that it stays" '[ $rc = 0 ] && [ "$(posts)" = 0 ] && grep -q "requests itself" "$T/err"'
reset; run remove --node runner-1 --tag tag:aaa
check "remove without --yes: exit 1, no POST" '[ $rc = 1 ] && [ "$(posts)" = 0 ]'

# --- validation (exit 2)
reset; run add --node twin --tag tag:task-runner --yes
check "ambiguous given name exits 2 without a POST" '[ $rc = 2 ] && [ "$(posts)" = 0 ] && grep -q "2 nodes" "$T/err"'
reset; run add --node nobody --tag tag:task-runner --yes
check "missing node exits 2" '[ $rc = 2 ] && [ "$(posts)" = 0 ]'
for t in 'Tag:x' 'tag:' 'tag:-x' 'tag:a_b' 'task-runner' "tag:$(printf 'a%.0s' $(seq 1 64))" 'tag:x
tag:y'; do
  reset; run add --node runner-1 --tag "$t" --yes
  check "bad tag '$(printf '%s' "$t" | head -c 20 | tr '\n' ' ')' exits 2 before any request" '[ $rc = 2 ] && [ ! -s "$CALLS" ]'
done
reset; run show --node runner-1 --tag tag:x
check "show with --tag is a usage error" '[ $rc = 2 ]'
reset; run add --node runner-1
check "add without --tag is a usage error" '[ $rc = 2 ]'
reset; run frob --node runner-1
check "unknown command is a usage error" '[ $rc = 2 ]'
reset; runenv HEADSCALE_URL=-x -- show --node runner-1
check "option-like HEADSCALE_URL exits 2" '[ $rc = 2 ] && [ ! -s "$CALLS" ]'
reset; runenv HEADSCALE_URL= -- show --node runner-1
check "missing HEADSCALE_URL exits 2" '[ $rc = 2 ]'
reset; runenv HEADSCALE_API_KEY= -- show --node runner-1
check "missing key exits 2" '[ $rc = 2 ] && [ ! -s "$CALLS" ]'
reset; runenv 'HEADSCALE_API_KEY=bad"key' -- show --node runner-1
check "a key with a quote is refused and not shown" '[ $rc = 2 ] && ! grep -q "bad" "$T/err" && [ ! -s "$CALLS" ]'

# --- HEADSCALE_API_KEY_CMD
reset; runenv EXPECT_KEY="$CANARY2" HEADSCALE_API_KEY=wrong-key HEADSCALE_API_KEY_CMD="cat '$KEYDIR/key'" -- add --node runner-1 --tag tag:task-runner --yes
check "HEADSCALE_API_KEY_CMD supplies the key and wins over HEADSCALE_API_KEY" '[ $rc = 0 ] && [ "$(forced 1)" = "[\"tag:aaa\",\"tag:exit\",\"tag:task-runner\"]" ]'
leak_check "the command's key is nowhere in output, argv or temp files"
reset; runenv HEADSCALE_API_KEY_CMD="echo $CANARY2; exit 1" -- show --node runner-1
check "a failing HEADSCALE_API_KEY_CMD exits 2 without echoing its output" '[ $rc = 2 ] && ! grep -q "$CANARY2" "$T/out" "$T/err" && [ ! -s "$CALLS" ]'

# --- remote failures (exit 3)
reset; : > "$FIX/noop"; run add --node runner-1 --tag tag:task-runner --yes
check "failed verification exits 3" '[ $rc = 3 ] && [ "$(posts)" = 1 ] && grep -q "not \[tag:aaa tag:exit tag:task-runner\]" "$T/err"'
reset; printf 403 > "$FIX/status"; run add --node runner-1 --tag tag:task-runner --yes
check "403 exits 3" '[ $rc = 3 ] && grep -q "HTTP 403" "$T/err"'
reset; runenv EXPECT_KEY=other -- show --node runner-1
check "a rejected key (401) exits 3" '[ $rc = 3 ] && grep -q "HTTP 401" "$T/err"'
leak_check "no leak on the refusal paths"
reset; : > "$FIX/unreachable"; run show --node runner-1
check "unreachable headscale exits 3" '[ $rc = 3 ]'

# --- legacy extras
reset; : > "$FIX/snake"; run add --node runner-1 --tag tag:task-runner --yes
check "legacy snake_case: add --yes keeps the forced tags and verifies" '[ $rc = 0 ] && [ "$(forced 1)" = "[\"tag:aaa\",\"tag:exit\",\"tag:task-runner\"]" ]'
reset; run add --node runner-1 --tag tag:self
check "legacy add of a requested-only tag notes it becomes admin-held" '[ $rc = 1 ] && grep -q "admin-held" "$T/err"'
reset; printf '%s' '[{"id":7,"name":"h","givenName":"num","forcedTags":["tag:a"],"validTags":[]}]' > "$FIX/nodes.json"
run add --node num --tag tag:task-runner --yes
check "a numeric id works (route and read-back)" '[ $rc = 0 ] && grep -qx "POST /api/v1/node/7/tags" "$CALLS" && [ "$(forced 7)" = "[\"tag:a\",\"tag:task-runner\"]" ]'

# --- unified model (headscale 0.28+: one tags list)
UNODES='[
 {"id":"11","name":"h1","givenName":"u-tagged","tags":["tag:exit","tag:aaa"]},
 {"id":"12","name":"h2","givenName":"u-owned","tags":[]},
 {"id":"13","name":"h3","givenName":"u-one","tags":["tag:task-runner"]},
 {"id":"14","name":"h4","givenName":"odd"}
]'
ureset() { reset; printf '%s' "$UNODES" > "$FIX/nodes.json"; }
ureset; run show --node u-tagged
check "unified: show prints the model and the tags list" '[ $rc = 0 ] && grep -q "^model: unified" "$T/out" && grep -qx "tags (admin-held after registration): \[tag:aaa tag:exit\]" "$T/out" && ! grep -q "^forced" "$T/out"'
ureset; run add --node u-tagged --tag tag:task-runner
check "unified: add without --yes plans on tags, exit 1, no POST" '[ $rc = 1 ] && [ "$(posts)" = 0 ] && grep -qx "tags: \[tag:aaa tag:exit\] -> \[tag:aaa tag:exit tag:task-runner\]" "$T/out"'
ureset; run add --node u-tagged --tag tag:task-runner --yes
check "unified: add --yes on a tagged node keeps the other tags and verifies" '[ $rc = 0 ] && [ "$(jq -c . "$FIX/lastbody")" = "{\"tags\":[\"tag:aaa\",\"tag:exit\",\"tag:task-runner\"]}" ] && [ "$(utags 11)" = "[\"tag:aaa\",\"tag:exit\",\"tag:task-runner\"]" ]'
leak_check "unified: the key is nowhere in output, argv or temp files"
ureset; run add --node u-owned --tag tag:task-runner
check "unified: first tag without --yes: plan with the ownership warning, exit 1, no POST" '[ $rc = 1 ] && [ "$(posts)" = 0 ] && grep -q "REMOVES that user ownership" "$T/out" && grep -q "allow-ownership-change" "$T/err"'
ureset; run add --node u-owned --tag tag:task-runner --yes
check "unified: first tag with --yes only exits 2, no POST" '[ $rc = 2 ] && [ "$(posts)" = 0 ] && grep -q "allow-ownership-change" "$T/err"'
ureset; run add --node u-owned --tag tag:task-runner --yes --allow-ownership-change
check "unified: first tag with --yes and --allow-ownership-change applies" '[ $rc = 0 ] && [ "$(utags 12)" = "[\"tag:task-runner\"]" ]'
ureset; run remove --node u-tagged --tag tag:exit --yes
check "unified: remove keeps the rest" '[ $rc = 0 ] && [ "$(utags 11)" = "[\"tag:aaa\"]" ]'
ureset; run remove --node u-one --tag tag:task-runner --yes
check "unified: removing the last tag is refused without the flag (exit 2, no POST)" '[ $rc = 2 ] && [ "$(posts)" = 0 ] && grep -q "re-authentication" "$T/err"'
ureset; run remove --node u-one --tag tag:task-runner
check "unified: removing the last tag without --yes is refused too (exit 2)" '[ $rc = 2 ] && [ "$(posts)" = 0 ]'
ureset; run remove --node u-one --tag tag:task-runner --yes --allow-ownership-change
check "unified: removing the last tag with the flag is sent" '[ $rc = 0 ] && [ "$(jq -c . "$FIX/lastbody")" = "{\"tags\":[]}" ]'
ureset; run add --node u-one --tag tag:task-runner --yes
check "unified: add is idempotent (no POST)" '[ $rc = 0 ] && [ "$(posts)" = 0 ] && grep -q "(no change)" "$T/out"'
ureset; : > "$FIX/noop"; run add --node u-tagged --tag tag:task-runner --yes
check "unified: failed verification exits 3" '[ $rc = 3 ] && [ "$(posts)" = 1 ]'
ureset; run show --node odd
check "unrecognised node shape (no forced field, no tags) exits 2" '[ $rc = 2 ] && grep -q "unrecognised node shape" "$T/err"'
ureset; run add --node odd --tag tag:task-runner --yes
check "unrecognised node shape: add exits 2 without a POST" '[ $rc = 2 ] && [ "$(posts)" = 0 ]'
reset; run show --node runner-1 --allow-ownership-change
check "show with --allow-ownership-change is a usage error" '[ $rc = 2 ]'

# --- key hygiene
printf '%s\r\n' "$CANARY2" > "$KEYDIR/key-crlf"
reset; runenv EXPECT_KEY="$CANARY2" HEADSCALE_API_KEY_CMD="cat '$KEYDIR/key-crlf'" -- show --node runner-1
check "a trailing CR from HEADSCALE_API_KEY_CMD is stripped" '[ $rc = 0 ]'
reset; runenv "HEADSCALE_API_KEY=$(printf 'abc\rdef')" -- show --node runner-1
check "a key with an inner CR is rejected before any request" '[ $rc = 2 ] && grep -q "unusable API key" "$T/err" && [ ! -s "$CALLS" ]'
reset; runenv 'HEADSCALE_API_KEY=ab$cd' -- show --node runner-1
check "a key with a shell character is rejected" '[ $rc = 2 ] && [ ! -s "$CALLS" ]'

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

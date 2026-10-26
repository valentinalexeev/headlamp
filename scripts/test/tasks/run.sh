#!/bin/sh
# Tests for scripts/tasks.sh. No network: curl and sops are fakes on PATH (curl is a small stateful node-control);
# jq is real.
#   sh scripts/test/tasks/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../tasks.sh"
if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq is not installed; the tasks client tests need it"
  exit 0
fi
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
BIN="$T/bin"; FIX="$T/fix"; CALLS="$T/calls"; ARGV="$T/argv"; LEAK="$T/leak"; FIRSTARG="$T/firstarg"; SCRATCH="$T/tmp"
SOPSFIX="$T/sopsfix"   # the canned decrypted output lives here, outside every place that is scanned
mkdir -p "$BIN" "$SOPSFIX"

CANARY='canary-s3cr3t-value'

# Fake curl: a tiny stateful node-control. State is $FIX/state.json = {config, scripts, secrets:[names]}.
# Overrides: $FIX/force.<METHOD>_<key>.{status,body} answers instead; $FIX/noapply makes the real PUT /tasks/config a no-op.
# Every call logs "METHOD /path" and its argv, and scans $TMPDIR for the canary (a secret on disk during the run is a leak).
cat > "$BIN/curl" <<'STUB'
#!/bin/sh
printf '%s\n' "$@" >> "$ARGV"
printf '%s\n' "$1" >> "$FIRSTARG"
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
echo "$method $path" >> "$CALLS"
if grep -rq -e "$CANARY" "$TMPDIR" 2>/dev/null; then echo "curl $method $path" >> "$LEAK"; fi
[ -e "$FIX/unreachable" ] && { echo "curl: (7) failed to connect" >&2; exit 7; }
[ -f "$FIX/state.json" ] || echo '{"config":null,"scripts":{},"secrets":[]}' > "$FIX/state.json"
reply() { # STATUS [BODY-FILE]
  [ -n "$out" ] && { if [ -n "${2:-}" ]; then cp "$2" "$out"; else : > "$out"; fi; }
  printf '%s' "$1"; exit 0
}
key=$(printf '%s' "${method}_${path#/}" | sed 's/?.*//' | tr / _)
if [ -e "$FIX/force.$key.status" ]; then reply "$(cat "$FIX/force.$key.status")" "$FIX/force.$key.body"; fi
r="$FIX/reply.json"
case "$method ${path%%\?*}" in
  "GET /tasks/config")
    if [ "$(jq '.config' "$FIX/state.json")" = null ]; then echo '{"error":"no config"}' > "$r"; reply 404 "$r"; fi
    jq '{config, scripts}' "$FIX/state.json" > "$r"; reply 200 "$r" ;;
  "PUT /tasks/config")
    dry=false; case "$path" in *dry_run=1*) dry=true ;; esac
    if [ "$body" = - ]; then body="$FIX/stdin.body"; cat > "$body"; fi
    jq -n --slurpfile b "$body" --slurpfile s "$FIX/state.json" --argjson dry "$dry" '
      $b[0] as $b | $s[0] as $s
      | ($b.config.tasks // {}) as $nt | ($s.config.tasks // {}) as $ot
      | def d($n; $o): {added: ([$n|keys[] | select($o[.] == null)]),
                        changed: ([$n|keys[] | select($o[.] != null and $o[.] != $n[.])]),
                        removed: ([$o|keys[] | select($n[.] == null)])};
      {problems: ([ $nt | to_entries[] | .key as $id | .value as $t
          | (select($b.scripts[$t.script] == null) | "task \($id): script \($t.script) is missing"),
            (($t.secrets // [])[] | select(. as $x | $s.secrets | index($x) | not)
              | "task \($id): secret \(.) is not stored (push secrets first)") ])} as $p
      | if ($p.problems | length) > 0 then {status: 422, body: {error: "invalid config", problems: $p.problems}}
        else {status: 200, body: ({tasks: d($nt; $ot), scripts: d($b.scripts; $s.scripts), dry_run: $dry})} end' > "$FIX/result.json"
    st=$(jq -r '.status' "$FIX/result.json"); jq '.body' "$FIX/result.json" > "$r"
    if [ "$st" = 200 ] && [ "$dry" = false ] && [ ! -e "$FIX/noapply" ]; then
      jq --slurpfile b "$body" '.config = $b[0].config | .scripts = $b[0].scripts' "$FIX/state.json" > "$FIX/state.new"
      mv "$FIX/state.new" "$FIX/state.json"
    fi
    reply "$st" "$r" ;;
  "PUT /tasks/secrets")
    # the body is read from stdin only; just its names, sizes and checksum are kept
    cat > "$FIX/secrets.in"
    cksum < "$FIX/secrets.in" > "$FIX/secrets.cksum"
    jq -c 'map_values(length)' "$FIX/secrets.in" > "$FIX/secrets.lengths" 2>/dev/null
    rm -f "$FIX/secrets.in"
    jq -n --slurpfile s "$FIX/state.json" --slurpfile l "$FIX/secrets.lengths" '{stored: ($l[0] | keys)}' > "$r"
    jq --slurpfile l "$FIX/secrets.lengths" '.secrets = ((.secrets + ($l[0] | keys)) | unique)' "$FIX/state.json" > "$FIX/state.new"
    mv "$FIX/state.new" "$FIX/state.json"
    reply 200 "$r" ;;
esac
echo "curl: (7) no route for $method $path" >&2; exit 7
STUB

# Fake sops: prints the canned decrypted JSON; records only its arguments.
cat > "$BIN/sops" <<'STUB'
#!/bin/sh
printf 'sops %s\n' "$*" >> "$ARGV"
echo sops >> "$CALLS"
n=0; [ -f "$FIX/sops.count" ] && n=$(cat "$FIX/sops.count"); n=$((n + 1)); echo "$n" > "$FIX/sops.count"
[ -e "$FIX/sops.fail.$n" ] && { echo "sops: failed to decrypt" >&2; exit 1; }
if grep -rq -e "$CANARY" "$TMPDIR" 2>/dev/null; then echo "sops" >> "$LEAK"; fi
cat "$SOPSOUT"
STUB
chmod +x "$BIN/curl" "$BIN/sops"

export FIX CALLS ARGV FIRSTARG LEAK CANARY TMPDIR="$SCRATCH" SOPSOUT="$SOPSFIX/out.json" PATH="$BIN:$PATH"
export NODE_CONTROL_URL="http://nc.example.test/"
unset TASKS_FILE TASKS_SCRIPTS_DIR TASKS_SECRETS_FILE
mkdir -p "$SCRATCH"

pass=0; fails=0
ok()  { echo "ok   - $1"; pass=$((pass + 1)); }
bad() { echo "FAIL - $1"; fails=$((fails + 1)); }
has() { grep -q -e "$1" "$2"; }
run() { sh "$script" "$@" > "$T/out" 2> "$T/err" < /dev/null; rc=$?; }
reset() { rm -rf "$FIX"; mkdir -p "$FIX"; : > "$CALLS"; : > "$ARGV"; : > "$LEAK"; : > "$FIRSTARG"; }
order() { tr '\n' ',' < "$CALLS"; }
nputs() { grep -c '^PUT /tasks/config' "$CALLS" || true; }

W="$T/work"; mkdir -p "$W/scripts"
CFG="$W/tasks.json"; SD="$W/scripts"
# a script with quotes, a newline, a backslash and a non-ASCII letter; no trailing newline on the second one
printf 'const s = "it'"'"'s \\"quoted\\"";\n// caf\303\251 \\n done\n' > "$SD/check-balance.js"
printf 'export default 1' > "$SD/other.js"
cat > "$CFG" <<'JSON'
{"tasks": {
  "balance": {"script": "check-balance.js", "every": "6h", "secrets": ["API_TOKEN"]},
  "other": {"script": "other.js", "every": "1h"}
}}
JSON
printf '{"API_TOKEN":"%s","PING_URL":"https://example.com/ping/%s"}' "$CANARY" "$CANARY" > "$SOPSFIX/out.json"
printf 'secrets: encrypted-placeholder\n' > "$W/secrets.sops.yaml"
SEC="$W/secrets.sops.yaml"
A="--file $CFG --scripts $SD"

# --- check
reset
run check $A
{ [ $rc = 2 ] && has 'secret API_TOKEN is not stored' "$T/err"; } && ok "check without the secret stored exits 2 and shows the server's problems" || bad "check unstored rc=$rc: $(cat "$T/err")"

reset; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
run check $A
{ [ $rc = 0 ] && has '^ok: tasks +2 ~0 -0, scripts +2 ~0 -0' "$T/out" && [ "$(order)" = "PUT /tasks/config?dry_run=1," ]; } && ok "check ok exits 0 with counts, one dry-run call" || bad "check ok rc=$rc out=$(cat "$T/out") calls=$(order)"
[ ! "$(jq -r '.config.tasks | length' "$FIX/state.json")" = 2 ] && ok "check stores nothing" || bad "check stored the config"

reset
printf '%s' '{"error":"invalid config","problems":["task balance: every must be at least 1m","task x: unknown field"]}' > "$FIX/force.PUT_tasks_config.body"; printf 422 > "$FIX/force.PUT_tasks_config.status"
run check $A
{ [ $rc = 2 ] && has 'every must be at least 1m' "$T/err" && has 'task x: unknown field' "$T/err" && has 'HTTP 422' "$T/err"; } && ok "check reports a 422 body with its problems, exit 2" || bad "check 422 rc=$rc: $(cat "$T/err")"

reset; printf 413 > "$FIX/force.PUT_tasks_config.status"; printf '%s' '{"error":"script too large"}' > "$FIX/force.PUT_tasks_config.body"
run check $A
{ [ $rc = 2 ] && has 'script too large' "$T/err"; } && ok "check 413 exits 2" || bad "check 413 rc=$rc"

reset; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
mv "$SD/other.js" "$SD/other.js.off"
run check $A
{ [ $rc = 2 ] && has 'script not found: other.js' "$T/err" && [ ! -s "$CALLS" ]; } && ok "check with a missing local script exits 2 naming it, sends nothing" || bad "check missing script rc=$rc: $(cat "$T/err")"
mv "$SD/other.js.off" "$SD/other.js"

printf 'unused' > "$SD/unreferenced.js"
reset; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
run check $A
has '^ok: tasks +2 ~0 -0, scripts +2 ~0 -0' "$T/out" && ok "only the referenced scripts are sent (an unreferenced file is ignored)" || bad "unreferenced script was sent: $(cat "$T/out")"
rm -f "$SD/unreferenced.js"

echo '{"tasks":{"t":{"script":"../evil.js","every":"1h"}}}' > "$W/evil.json"
reset
run check --file "$W/evil.json" --scripts "$SD"
{ [ $rc = 2 ] && has 'not a plain file name' "$T/err" && [ ! -s "$CALLS" ]; } && ok "a task naming ../evil.js exits 2 locally" || bad "evil name rc=$rc"

reset; run check --file "$W/none.json" --scripts "$SD"
[ $rc = 2 ] && ok "missing config file exits 2" || bad "missing config rc=$rc"
printf '[1]' > "$W/arr.json"; run check --file "$W/arr.json" --scripts "$SD"
[ $rc = 2 ] && ok "a config that is not an object exits 2" || bad "array config rc=$rc"

# --- push, diff
mk_ready() { reset; echo '{"config":null,"scripts":{},"secrets":[]}' > "$FIX/state.json"; }
mk_ready; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
run diff $A
{ [ $rc = 1 ] && has '^+ task balance$' "$T/out" && has '^+ task other$' "$T/out" && has '^+ script check-balance.js$' "$T/out" && has '^+ script other.js$' "$T/out"; } \
  && ok "diff exits 1 and lists + task/script lines" || bad "diff before push rc=$rc out=$(cat "$T/out")"

: > "$CALLS"
run push $A --yes
{ [ $rc = 0 ] && has 'stored: 2 tasks' "$T/err" && has 'verified' "$T/err"; } && ok "push --yes stores the config and verifies" || bad "push rc=$rc: $(cat "$T/err")"
[ "$(order)" = "PUT /tasks/config?dry_run=1,PUT /tasks/config,PUT /tasks/config?dry_run=1," ] && ok "push order: dry run, real put, verifying dry run" || bad "push order: $(order)"
jq -e --rawfile s "$SD/check-balance.js" '.scripts["check-balance.js"] == $s' "$FIX/state.json" >/dev/null && ok "the stored script is byte-exact (quotes, newline, backslash, non-ASCII)" || bad "stored script differs"

run diff $A
{ [ $rc = 0 ] && [ ! -s "$T/out" ]; } && ok "diff exits 0 and prints nothing after the push" || bad "diff after push rc=$rc out=$(cat "$T/out")"

: > "$CALLS"
run push $A --yes
{ [ $rc = 0 ] && has 'nothing to do' "$T/err" && [ "$(nputs)" = 1 ] && [ "$(order)" = "PUT /tasks/config?dry_run=1," ]; } && ok "pushing the same content again is a no-op" || bad "second push rc=$rc calls=$(order)"

printf 'export default 2' > "$SD/other.js"
run diff $A
{ [ $rc = 1 ] && has '^~ script other.js$' "$T/out"; } && ok "a changed script shows as ~" || bad "changed diff: $(cat "$T/out")"
printf 'export default 1' > "$SD/other.js"
jq 'del(.tasks.other)' "$CFG" > "$W/c2.json"
run diff --file "$W/c2.json" --scripts "$SD"
{ [ $rc = 1 ] && has '^- task other$' "$T/out" && has '^- script other.js$' "$T/out"; } && ok "a removed task and script show as -" || bad "removed diff: $(cat "$T/out")"

# non-interactive push without --yes
mk_ready; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
run push $A
{ [ $rc = 2 ] && has 'without --yes' "$T/err" && [ "$(nputs)" = 1 ]; } && ok "non-interactive push without --yes exits 2 and sends no config" || bad "no-yes rc=$rc calls=$(order)"

# --- secrets
secret_leak() { # DESCRIPTION: the canary must not be in stdout, stderr, argv, the call log or any file left in TMPDIR
  _l=0
  for f in "$T/out" "$T/err" "$ARGV" "$CALLS" "$LEAK"; do grep -q -e "$CANARY" "$f" && _l=1; done
  grep -rq -e "$CANARY" "$SCRATCH" && _l=1
  [ -s "$LEAK" ] && _l=1
  # nothing under the fake server's directory either (it keeps names, lengths and a checksum only)
  grep -rq -e "$CANARY" "$FIX" && _l=1
  [ $_l = 0 ] && ok "$1" || bad "$1 (the canary leaked)"
}
mk_ready
run push $A --secrets "$SEC" --yes
{ [ $rc = 0 ] && has 'secrets stored: API_TOKEN, PING_URL' "$T/err" && has 'stored: 2 tasks' "$T/err"; } && ok "push --secrets stores secrets and config, naming only the names" || bad "push secrets rc=$rc: $(cat "$T/err")"
case $(order) in "sops,PUT /tasks/config?dry_run=1,sops,PUT /tasks/secrets,PUT /tasks/config?dry_run=1,PUT /tasks/config,PUT /tasks/config?dry_run=1,"*) ok "push order: secrets before the real config" ;; *) bad "secrets order: $(order)" ;; esac
secret_leak "decrypted secrets never reach stdout, stderr, argv, temp files or the fake server's disk"
{ grep -qx 'sops -d --output-type json '"$SEC" "$ARGV" && [ "$(cat "$FIX/secrets.cksum")" = "$(cksum < "$SOPSFIX/out.json")" ]; } && ok "curl got exactly sops' output on stdin (sops -d --output-type json FILE)" || bad "secrets body mismatch"
grep -qx -e '@-' "$ARGV" && ok "curl reads the secrets body from stdin (--data-binary @-)" || bad "no @- in argv"
[ "$(cat "$FIX/secrets.lengths")" = "$(jq -c 'map_values(length)' "$SOPSFIX/out.json")" ] && ok "the server saw both secret values in full" || bad "secret lengths differ"

: > "$CALLS"
run push $A --secrets "$SEC" --yes
{ [ $rc = 0 ] && has 'secrets stored' "$T/err" && [ "$(nputs)" = 1 ]; } && ok "pushing again with --secrets re-sends the secrets but not the config" || bad "second secrets push rc=$rc calls=$(order)"
run push $A --yes
{ [ $rc = 0 ] && has 'nothing to do' "$T/err"; } && ok "without --secrets an identical push is a no-op" || bad "no secrets noop rc=$rc"
secret_leak "no leak on the repeated pushes either"

# env var for the secrets file, and flags winning over the env
mk_ready
TASKS_SECRETS_FILE="$SEC" run push $A --yes
has 'secrets stored' "$T/err" && ok "TASKS_SECRETS_FILE selects the sops file" || bad "env secrets file"

# validation of the decrypted shape
for shape in '{"A":{"nested":"x"}}' '{"A":""}' '{"A":1}' '{"bad name":"x"}' '[]' '{}' 'not json'; do
  mk_ready; printf '%s' "$shape" > "$SOPSFIX/out.json"
  run push $A --secrets "$SEC" --yes
  { [ $rc = 2 ] && ! has 'PUT /tasks' "$CALLS"; } && ok "secrets shape $shape: exit 2, nothing sent" || bad "secrets shape $shape rc=$rc calls=$(order)"
done
printf '%s' '{"A":"v"}' > "$SOPSFIX/out.json"
mk_ready; run push $A --secrets "$W/absent.yaml" --yes
{ [ $rc = 2 ] && ! has 'PUT' "$CALLS"; } && ok "a missing sops file exits 2" || bad "absent sops rc=$rc"
printf '%s' "{\"API_TOKEN\":\"$CANARY\",\"N\":{\"x\":\"$CANARY\"}}" > "$SOPSFIX/out.json"
mk_ready; run push $A --secrets "$SEC" --yes
{ [ $rc = 2 ] && ! has 'PUT /tasks' "$CALLS"; } && ok "a nested value beside a good one is exit 2" || bad "nested rc=$rc"
secret_leak "a rejected secrets file does not leak either"
printf '{"API_TOKEN":"%s"}' "$CANARY" > "$SOPSFIX/out.json"

# --- pull
mk_ready; run push $A --secrets "$SEC" --yes
P="$T/pulled"; rm -rf "$P"; mkdir -p "$P"
run pull --file "$P/tasks.json" --scripts "$P/scripts"
{ [ $rc = 0 ] && jq -S . "$CFG" | cmp -s - "$P/tasks.json" && cmp -s "$SD/check-balance.js" "$P/scripts/check-balance.js" && cmp -s "$SD/other.js" "$P/scripts/other.js"; } \
  && ok "pull writes the config (sorted JSON) and the scripts byte-exact" || bad "pull rc=$rc: $(cat "$T/err")"
[ -z "$(grep -rl -e "$CANARY" "$P" "$T/out" "$T/err" 2>/dev/null)" ] && [ "$(ls "$P")" = "scripts
tasks.json" ] && ok "pull writes no secrets (only the config file and scripts)" || bad "pull wrote something else: $(ls -A "$P")"
run pull --file "$P/tasks.json" --scripts "$P/scripts"
[ $rc = 0 ] && ok "pulling identical content again needs no --yes" || bad "re-pull rc=$rc"

printf 'locally edited' > "$P/scripts/other.js"
run pull --file "$P/tasks.json" --scripts "$P/scripts"
{ [ $rc = 2 ] && has "$P/scripts/other.js" "$T/err" && [ "$(cat "$P/scripts/other.js")" = 'locally edited' ]; } && ok "pull refuses to overwrite a differing file, names it, changes nothing" || bad "pull refuse rc=$rc: $(cat "$T/err")"
run pull --file "$P/tasks.json" --scripts "$P/scripts" --yes
{ [ $rc = 0 ] && cmp -s "$SD/other.js" "$P/scripts/other.js"; } && ok "pull --yes overwrites" || bad "pull --yes rc=$rc"

reset; echo '{"config":{"tasks":{"t":{"script":"a.js","every":"1h"}}},"scripts":{"a.js":"ok","../x":"evil"},"secrets":[]}' > "$FIX/state.json"
H="$T/hostile"; rm -rf "$H" "$T/x"; mkdir -p "$H"
run pull --file "$H/tasks.json" --scripts "$H/scripts"
{ [ $rc = 3 ] && [ ! -e "$T/x" ] && [ ! -e "$H/tasks.json" ] && [ ! -e "$H/scripts" ] && [ ! -e "$H/x" ]; } && ok "pull refuses a hostile script name (../x): exit 3, nothing written" || bad "hostile rc=$rc: $(ls -A "$H")"
for evil in '..' '.' 'a/b' 'a
b' ''; do
  reset; jq -n --arg n "$evil" '{config:{tasks:{}},scripts:{($n):"x"},secrets:[]}' > "$FIX/state.json"
  run pull --file "$H/tasks.json" --scripts "$H/scripts"
  { [ $rc = 3 ] && [ ! -e "$H/tasks.json" ]; } && ok "pull refuses the script name $(printf '%s' "$evil" | jq -Rs .)" || bad "hostile name $evil rc=$rc"
done
reset; run pull --file "$H/tasks.json" --scripts "$H/scripts"
{ [ $rc = 3 ] && has 'no tasks config' "$T/err"; } && ok "pull on a server without a config exits 3" || bad "pull 404 rc=$rc"

# --- first push of secrets (pending path), review fixes
# runtty INPUT ARGS...: run the script on a pseudo terminal that types INPUT; needs python3.
runtty() {
  _in=$1; shift
  OUT="$T/out" ERR="$T/err" IN="$_in" python3 - "$script" "$@" <<'PY'
import os, pty, sys, time, signal
signal.alarm(30)
script, args = sys.argv[1], sys.argv[2:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp('sh', ['sh', '-c', 'exec sh "$0" "$@" >"$OUT" 2>"$ERR"', script] + args)
for line in os.environ['IN'].encode().decode('unicode_escape').splitlines(True):
    time.sleep(0.5)
    os.write(fd, line.encode())
try:
    while os.read(fd, 1024):
        pass
except OSError:
    pass
_, st = os.waitpid(pid, 0)
sys.exit(os.waitstatus_to_exitcode(st))
PY
  rc=$?
}
if command -v python3 >/dev/null 2>&1; then
  mk_ready; runtty 'y\ny\n' push $A --secrets "$SEC"
  { [ $rc = 0 ] && grep -q 'Send secrets and apply?' "$T/err" && grep -q 'Apply?' "$T/err" && has '^+ task balance$' "$T/out"; } && ok "pending push asks twice and shows the diff before the second question" || bad "pending tty rc=$rc: $(cat "$T/err")"
  case $(order) in "sops,PUT /tasks/config?dry_run=1,sops,PUT /tasks/secrets,PUT /tasks/config?dry_run=1,PUT /tasks/config,"*) ok "pending push: secrets sent between the two prompts, config after the second" ;; *) bad "pending order: $(order)" ;; esac
  has 'secrets not yet stored on node-control: API_TOKEN' "$T/err" && ok "the pending secret names are shown" || bad "pending names"
  mk_ready; runtty 'n\n' push $A --secrets "$SEC"
  { [ $rc = 2 ] && ! has 'PUT /tasks/secrets' "$CALLS" && ! has 'PUT /tasks/config$' "$CALLS"; } && ok "declining the first question sends nothing" || bad "decline first rc=$rc calls=$(order)"
  mk_ready; runtty 'y\nn\n' push $A --secrets "$SEC"
  { [ $rc = 2 ] && has 'PUT /tasks/secrets' "$CALLS" && ! has 'PUT /tasks/config$' "$CALLS"; } && ok "declining the second question keeps the secrets but sends no config" || bad "decline second rc=$rc calls=$(order)"
else
  echo "skip - python3 is not available: pty prompt tests"
fi
printf '{"API_TOKEN":"%s","PING_URL":"x"}' "$CANARY" > "$SOPSFIX/out.json"

mk_ready
run push $A --secrets "$SEC"
{ [ $rc = 2 ] && ! has 'PUT /tasks/secrets' "$CALLS" && ! has 'PUT /tasks/config$' "$CALLS"; } && ok "non-interactive pending push without --yes exits 2 before sending secrets" || bad "pending non-tty rc=$rc calls=$(order)"
has 'without --yes' "$T/err" && ok "...with the --yes message" || bad "pending non-tty message"

# another problem than a missing secret: nothing sent
mk_ready; jq '.tasks.balance.bogus = true' "$CFG" > "$W/bogus.json"
printf 422 > "$FIX/force.PUT_tasks_config.status"; printf '%s' '{"error":"invalid task config","problems":["task balance: bogus: unknown field","task balance: secret API_TOKEN is not stored (push secrets first)"]}' > "$FIX/force.PUT_tasks_config.body"
run push --file "$W/bogus.json" --scripts "$SD" --secrets "$SEC" --yes
{ [ $rc = 2 ] && has 'unknown field' "$T/err" && ! has 'PUT /tasks/secrets' "$CALLS"; } && ok "a non-secret problem beside a missing secret: exit 2, nothing sent" || bad "mixed problems rc=$rc calls=$(order)"

# missing secret name that the sops file does not hold
printf '%s' '{"OTHER":"v"}' > "$SOPSFIX/out.json"
mk_ready
run push $A --secrets "$SEC" --yes
{ [ $rc = 2 ] && has 'API_TOKEN' "$T/err" && has 'not in' "$T/err" && ! has 'PUT /tasks/secrets' "$CALLS"; } && ok "a missing secret that is not in the sops file: exit 2, nothing sent" || bad "name not in sops rc=$rc: $(cat "$T/err")"
printf '{"API_TOKEN":"%s","PING_URL":"x"}' "$CANARY" > "$SOPSFIX/out.json"

# curl never reads a user rc file: -q first on every call
mk_ready; : > "$FIRSTARG"; run push $A --secrets "$SEC" --yes
[ -s "$FIRSTARG" ] && [ "$(sort -u "$FIRSTARG")" = "-q" ] && ok "every curl call starts with -q (no ~/.curlrc)" || bad "first args: $(sort -u "$FIRSTARG")"
[ "$(grep -c . "$FIRSTARG")" = "$(grep -c '^PUT\|^GET' "$CALLS")" ] && ok "...for all $(grep -c . "$FIRSTARG") calls" || bad "curl call count mismatch"

# secrets answers
mk_ready; printf 422 > "$FIX/force.PUT_tasks_secrets.status"; printf '%s' '{"error":"a secret name is invalid"}' > "$FIX/force.PUT_tasks_secrets.body"
run push $A --secrets "$SEC" --yes
{ [ $rc = 2 ] && has 'secrets were not stored (HTTP 422): a secret name is invalid' "$T/err" && ! has 'refused the config' "$T/err" && ! has 'PUT /tasks/config$' "$CALLS"; } && ok "secrets 422: dedicated message, exit 2, no config sent" || bad "secrets 422 rc=$rc: $(cat "$T/err")"
mk_ready; printf 500 > "$FIX/force.PUT_tasks_secrets.status"; printf '{}' > "$FIX/force.PUT_tasks_secrets.body"
run push $A --secrets "$SEC" --yes
{ [ $rc = 3 ] && has 'secrets were not stored' "$T/err"; } && ok "secrets 500: exit 3" || bad "secrets 500 rc=$rc"
mk_ready; printf 200 > "$FIX/force.PUT_tasks_secrets.status"; printf '%s' '{"stored":["API_TOKEN"],"pending_write":true}' > "$FIX/force.PUT_tasks_secrets.body"
run push $A --secrets "$SEC" --yes
has 'has not yet written the secrets' "$T/err" && ok "pending_write on the secrets answer is surfaced" || bad "secrets pending_write"
mk_ready; : > "$FIX/sops.fail.2"
run push $A --secrets "$SEC" --yes
{ [ $rc = 2 ] && has 'sops failed while' "$T/err" && ! has 'PUT /tasks/config$' "$CALLS"; } && ok "sops failing while sending: exit 2, no config sent" || bad "sops fail rc=$rc: $(cat "$T/err")"
mk_ready; : > "$FIX/sops.fail.1"
run push $A --secrets "$SEC" --yes
{ [ $rc = 2 ] && has 'could not decrypt' "$T/err" && { [ ! -s "$CALLS" ] || [ "$(order)" = "sops," ]; }; } && ok "sops failing at validation: exit 2, nothing sent" || bad "sops fail1 rc=$rc"
secret_leak "no leak on any failure path"

# flags with no effect
for f in "check --secrets $SEC" "diff --secrets $SEC" "pull --secrets $SEC" "check --yes" "diff --yes"; do
  reset; run $f $A
  { [ $rc = 2 ] && [ ! -s "$CALLS" ]; } && ok "'$f' exits 2" || bad "'$f' rc=$rc"
done
reset; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"
TASKS_SECRETS_FILE="$SEC" run check $A
[ $rc = 0 ] && ok "TASKS_SECRETS_FILE stays silently ignored by check" || bad "env secrets on check rc=$rc"

# --- remote failures
reset; printf 403 > "$FIX/force.PUT_tasks_config.status"; echo '{"error":"forbidden"}' > "$FIX/force.PUT_tasks_config.body"
run check $A
[ $rc = 3 ] && ok "403 exits 3" || bad "403 rc=$rc"
reset; printf 503 > "$FIX/force.GET_tasks_config.status"; echo '{"error":"degraded"}' > "$FIX/force.GET_tasks_config.body"
run pull --file "$P/tasks.json" --scripts "$P/scripts"
[ $rc = 3 ] && ok "503 exits 3" || bad "503 rc=$rc"
reset; : > "$FIX/unreachable"
run diff $A
[ $rc = 3 ] && ok "an unreachable server exits 3" || bad "unreachable rc=$rc"
reset; echo '{"config":{"tasks":{}},"scripts":{},"secrets":["API_TOKEN"]}' > "$FIX/state.json"; : > "$FIX/noapply"
run push $A --yes
{ [ $rc = 3 ] && has 'live config differs after push' "$T/err"; } && ok "post-push mismatch exits 3" || bad "mismatch rc=$rc: $(cat "$T/err")"

# --- usage
run frobnicate
[ $rc = 2 ] && ok "unknown subcommand exits 2" || bad "unknown subcommand rc=$rc"
run
[ $rc = 2 ] && ok "no subcommand exits 2" || bad "no subcommand rc=$rc"
run check --bogus
[ $rc = 2 ] && ok "unknown option exits 2" || bad "unknown option rc=$rc"
(unset NODE_CONTROL_URL; sh "$script" check $A >/dev/null 2>&1); [ $? = 2 ] && ok "missing NODE_CONTROL_URL exits 2" || bad "missing url"
(NODE_CONTROL_URL=--evil sh "$script" check $A >/dev/null 2>&1); [ $? = 2 ] && ok "option-like NODE_CONTROL_URL exits 2" || bad "dash url"
(cd "$W" && TASKS_FILE="$CFG" TASKS_SCRIPTS_DIR="$SD" sh "$script" diff > "$T/out" 2>&1 < /dev/null); rc=$?
[ $rc != 2 ] && ok "TASKS_FILE and TASKS_SCRIPTS_DIR are honoured" || bad "env paths rc=$rc: $(cat "$T/out")"

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

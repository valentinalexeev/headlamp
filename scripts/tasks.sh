#!/bin/sh
# CLI client of node-control's scheduled-task API: check, diff, push and pull the task configuration, the scripts it
# references and (push only) the secrets they need. No credentials are used here: node-control identifies the caller by
# its tailnet identity, so run this from a tailnet node whose user is allowed to manage tasks.
#
#   tasks.sh check [--file F] [--scripts DIR]       validate the config and scripts on the server (dry run)
#   tasks.sh diff  [--file F] [--scripts DIR]       show what a push would add (+), change (~) and remove (-)
#                                                   (exit 1 when anything differs)
#   tasks.sh push  [--file F] [--scripts DIR] [--secrets SOPS_FILE] [--yes]
#                                                   check, show the diff, ask, send the secrets (when given), then the
#                                                   config and scripts, and verify that the live state equals the files
#   tasks.sh pull  [--file F] [--scripts DIR] [--yes]
#                                                   write the live config and scripts to the files (never secrets)
#
# The config file holds the object {"tasks": {...}}. Only the scripts that a task references (its `script` field) are
# read from DIR and sent; pull writes the same set. Secrets live in a sops file (a flat JSON object NAME -> value). They
# are decrypted by `sops -d` straight into the request through a pipe: the values never reach a file, an argument list
# or the terminal, and only their names are printed. Secrets go first, then the config (a task may only name secrets
# that are already stored).
#
# Environment (flags override the last three):
#   NODE_CONTROL_URL     base URL of node-control on the tailnet (required)
#   TASKS_FILE           config file, default tasks.json
#   TASKS_SCRIPTS_DIR    script directory, default scripts
#   TASKS_SECRETS_FILE   sops file for push, optional
#
# Exit codes: 0 ok / no diff; 1 diff present (diff only); 2 usage or validation failure (also server refusals
# 413/422, and a pull that would overwrite changed files without --yes); 3 remote failure (unreachable, 403, 5xx,
# a hostile answer, or the live config differing after a push). Every request is capped at 30 s.
set -eu

NAME_RE='\\A[A-Za-z0-9._-]{1,64}\\z'

say() { printf '%s\n' "$*" >&2; }
die() { code=$1; shift; say "tasks: $*"; exit "$code"; }
usage() { say "usage: tasks.sh {check|diff|push|pull} [--file FILE] [--scripts DIR] [--secrets SOPS_FILE] [--yes]"; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

command -v jq >/dev/null 2>&1 || die 2 "jq is required"

cmd=${1:-}
[ -n "$cmd" ] || usage
shift
case $cmd in check|diff|push|pull) ;; *) usage ;; esac

file=${TASKS_FILE:-tasks.json}
sdir=${TASKS_SCRIPTS_DIR:-scripts}
sfile=${TASKS_SECRETS_FILE:-}
yes=0
while [ $# -gt 0 ]; do
  case $1 in
    --file) [ $# -ge 2 ] || die 2 "--file needs a value"; file=$2; shift 2 ;;
    --scripts) [ $# -ge 2 ] || die 2 "--scripts needs a value"; sdir=$2; shift 2 ;;
    --secrets) [ $# -ge 2 ] || die 2 "--secrets needs a value"; sfile=$2; shift 2 ;;
    --yes) yes=1; shift ;;
    *) die 2 "unknown option: $1" ;;
  esac
done
[ "$cmd" = push ] || sfile=

url=${NODE_CONTROL_URL:-}
[ -n "$url" ] || die 2 "NODE_CONTROL_URL is required"
case $url in -*) die 2 "NODE_CONTROL_URL must not start with '-'" ;; esac
url=${url%/}

# req METHOD PATH [BODYFILE]: sets code (HTTP status) and leaves the answer in $tmp/resp; returns 1 on a transport failure.
req() {
  _m=$1; _p=$2; _b=${3:-}
  : > "$tmp/resp"
  if [ -n "$_b" ]; then
    code=$(curl --silent --show-error --max-time "${REQ_MAX_TIME:-30}" -X "$_m" -H 'Content-Type: application/json' \
      --data-binary @"$_b" -o "$tmp/resp" -w '%{http_code}' "$url$_p") || return 1
  else
    code=$(curl --silent --show-error --max-time "${REQ_MAX_TIME:-30}" -X "$_m" \
      -o "$tmp/resp" -w '%{http_code}' "$url$_p") || return 1
  fi
}

# remote METHOD PATH [BODYFILE]: like req, but a transport failure ends the run with exit 3.
remote() { req "$@" || die 3 "cannot reach node-control ($1 $2)"; }

detail() { jq -r '.detail // .error // empty' "$tmp/resp" 2>/dev/null || true; }

show_problems() {
  jq -r '(.problems // [])[]? | "  - " + (if type == "string" then . else tojson end)' "$tmp/resp" >&2 2>/dev/null || true
}

# refuse: an HTTP status that is not the expected one ends the run with the matching exit code.
refuse() {
  case $code in
    413|422)
      _d=$(detail); say "tasks: the server refused the config (HTTP $code)${_d:+: $_d}"
      show_problems; exit 2 ;;
    403) die 3 "node-control refused this identity (HTTP 403)" ;;
    503) die 3 "node-control is degraded (HTTP 503)" ;;
    *) die 3 "unexpected answer from node-control (HTTP $code)" ;;
  esac
}

# build_body: validate the config file and write $tmp/body = {config, scripts} with exactly the scripts it references.
build_body() {
  [ -f "$file" ] || die 2 "config file not found: $file"
  jq -e -s 'length == 1 and (.[0] | type == "object")' "$file" >/dev/null 2>&1 || die 2 "$file is not a single JSON object"
  jq -n --slurpfile c "$file" '{config: $c[0], scripts: {}}' > "$tmp/body"
  _refs='[.tasks // {} | .[]? | .script? | select(type == "string")] | unique'
  _bad=$(jq -r "$_refs | .[] | select(test(\"$NAME_RE\") and . != \".\" and . != \"..\" | not) | @json" "$file")
  [ -z "$_bad" ] || die 2 "a task names a script that is not a plain file name: $_bad"
  jq -r "$_refs | .[]" "$file" > "$tmp/refs"
  while IFS= read -r _n; do
    [ -f "$sdir/$_n" ] || die 2 "script not found: $_n (looked in $sdir)"
    jq --rawfile s "$sdir/$_n" --arg n "$_n" '.scripts[$n] = $s' "$tmp/body" > "$tmp/body.n"
    mv "$tmp/body.n" "$tmp/body"
  done < "$tmp/refs"
}

# plan [soft]: dry run of the body against the server; the answer goes to $tmp/plan.json. Returns 0, or (soft only) 10
# when the only problems are secrets that are not stored yet.
plan() {
  remote PUT '/tasks/config?dry_run=1' "$tmp/body"
  case $code in
    200) ;;
    422)
      if [ "${1:-}" = soft ] && jq -e '(.problems // []) | length > 0 and all(.[]; type == "string" and test("is not stored"))' \
           "$tmp/resp" >/dev/null 2>&1; then
        show_problems; return 10
      fi
      refuse ;;
    *) refuse ;;
  esac
  jq -e '(.tasks | type == "object") and (.scripts | type == "object")' "$tmp/resp" >/dev/null 2>&1 \
    || die 3 "node-control returned an unexpected plan"
  cp "$tmp/resp" "$tmp/plan.json"
}

JQ_N='def n(o; k): ((o[k] // []) | length);'
changes() {
  jq "$JQ_N [(.tasks, .scripts) | n(.; \"added\"), n(.; \"changed\"), n(.; \"removed\")] | add" "$tmp/plan.json"
}

summary() {
  jq -r "$JQ_N \"tasks +\\(n(.tasks; \"added\")) ~\\(n(.tasks; \"changed\")) -\\(n(.tasks; \"removed\")), \
scripts +\\(n(.scripts; \"added\")) ~\\(n(.scripts; \"changed\")) -\\(n(.scripts; \"removed\"))\"" "$tmp/plan.json"
}

show_diff() {
  jq -r 'def e(w; o): (o.added // [] | map(tostring) | sort | .[] | "+ " + w + " " + .),
                 (o.changed // [] | map(tostring) | sort | .[] | "~ " + w + " " + .),
                 (o.removed // [] | map(tostring) | sort | .[] | "- " + w + " " + .);
    e("task"; .tasks), e("script"; .scripts)' "$tmp/plan.json"
}

do_check() {
  build_body
  plan
  say_ok
}
say_ok() { printf 'ok: %s\n' "$(summary)"; }

do_diff() {
  build_body
  plan
  show_diff
  [ "$(changes)" = 0 ] || return 1
}

# Decrypted values only ever travel through pipes: sops -> jq (validation, no output) and sops -> curl.
SECRETS_OK='type == "object" and length > 0 and length <= 64 and (keys | all(test("\\A[A-Za-z0-9_]{1,64}\\z")))
  and all(.[]; type == "string" and length > 0)'

check_secrets() {
  [ -f "$sfile" ] || die 2 "secrets file not found: $sfile"
  command -v sops >/dev/null 2>&1 || die 2 "sops is required for --secrets"
  sops -d --output-type json "$sfile" | jq -e "$SECRETS_OK" >/dev/null 2>&1 \
    || die 2 "$sfile did not decrypt to a flat JSON object of non-empty string values with valid names (A-Z a-z 0-9 _)"
}

send_secrets() {
  code=$(sops -d --output-type json "$sfile" | curl --silent --show-error --max-time "${REQ_MAX_TIME:-30}" -X PUT \
    -H 'Content-Type: application/json' --data-binary @- -o "$tmp/resp" -w '%{http_code}' "$url/tasks/secrets") \
    || die 3 "could not send the secrets to node-control"
  [ "$code" = 200 ] || refuse
  jq -e '.stored | type == "array"' "$tmp/resp" >/dev/null 2>&1 || die 3 "node-control returned an unexpected answer to the secrets"
  say "secrets stored: $(jq -r '.stored | join(", ")' "$tmp/resp")"
}

do_push() {
  build_body
  [ -z "$sfile" ] || check_secrets
  _soft=; [ -z "$sfile" ] || _soft=soft
  _pend=0
  plan $_soft || _pend=1
  if [ "$_pend" = 0 ]; then
    show_diff
    say_ok
    if [ "$(changes)" = 0 ] && [ -z "$sfile" ]; then say "nothing to do"; return 0; fi
  else
    say "some secrets are not stored yet; they will be sent first"
  fi
  if [ "$yes" = 0 ]; then
    if [ -t 0 ]; then
      printf 'Apply? [y/N] ' >&2
      read -r _a || _a=
      case $_a in y|Y|yes) ;; *) die 2 "not applied" ;; esac
    else
      die 2 "refusing to push without --yes"
    fi
  fi

  [ -z "$sfile" ] || send_secrets

  if [ "$_pend" = 1 ]; then
    plan
    show_diff
    say_ok
  fi
  if [ "$(changes)" = 0 ]; then
    say "the config and scripts are already live"
    return 0
  fi
  remote PUT /tasks/config "$tmp/body"
  [ "$code" = 200 ] || refuse
  if [ "$(jq -r '.pending_write // false' "$tmp/resp" 2>/dev/null)" = true ]; then
    say "warning: the server has not yet written the config to its store; it keeps retrying"
  fi
  say "stored: $(jq '.config.tasks | length' "$tmp/body") tasks"

  plan
  [ "$(changes)" = 0 ] || { show_diff; die 3 "the live config differs after push"; }
  say "verified: the live config equals the files"
}

# write_file SRC DEST: replace DEST with SRC through a temp file in the same directory, keeping the mode of DEST.
write_file() {
  _dir=$(dirname "$2")
  mkdir -p "$_dir" || die 3 "cannot create $_dir"
  _t=$(mktemp "$_dir/.$(basename "$2").XXXXXX") || die 3 "cannot create a temporary file in $_dir"
  if [ -f "$2" ]; then cp -p "$2" "$_t"; else chmod 644 "$_t"; fi
  if cat "$1" > "$_t" && mv "$_t" "$2"; then
    say "wrote $2"
  else
    rm -f "$_t"
    die 3 "could not write $2"
  fi
}

do_pull() {
  remote GET /tasks/config
  [ "$code" != 404 ] || die 3 "node-control has no tasks config"
  [ "$code" = 200 ] || refuse
  jq -e '(.config | type == "object") and (.scripts | type == "object") and (.scripts | all(.[]; type == "string"))' \
    "$tmp/resp" >/dev/null 2>&1 || die 3 "node-control returned an unexpected config"
  _bad=$(jq -r ".scripts | keys[] | select(test(\"$NAME_RE\") and . != \".\" and . != \"..\" | not) | @json" "$tmp/resp")
  [ -z "$_bad" ] || die 3 "node-control returned a script name that is not a plain file name: $_bad; nothing was written"

  jq -S '.config' "$tmp/resp" > "$tmp/new.config"
  jq -r '.scripts | keys[]' "$tmp/resp" > "$tmp/names"
  mkdir -p "$tmp/new"
  : > "$tmp/changed"
  if [ -f "$file" ] && ! cmp -s "$file" "$tmp/new.config"; then printf '%s\n' "$file" >> "$tmp/changed"; fi
  while IFS= read -r _n; do
    jq -j --arg n "$_n" '.scripts[$n]' "$tmp/resp" > "$tmp/new/$_n"
    if [ -f "$sdir/$_n" ] && ! cmp -s "$sdir/$_n" "$tmp/new/$_n"; then printf '%s\n' "$sdir/$_n" >> "$tmp/changed"; fi
  done < "$tmp/names"
  if [ -s "$tmp/changed" ] && [ "$yes" = 0 ]; then
    say "these files differ from the live state and would be overwritten:"
    sed 's/^/  /' "$tmp/changed" >&2
    die 2 "refusing to overwrite them without --yes"
  fi

  write_file "$tmp/new.config" "$file"
  while IFS= read -r _n; do
    write_file "$tmp/new/$_n" "$sdir/$_n"
  done < "$tmp/names"
}

case $cmd in
  check) do_check ;;
  diff) do_diff ;;
  push) do_push ;;
  pull) do_pull ;;
esac

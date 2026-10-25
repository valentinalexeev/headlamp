#!/bin/sh
# CLI client of the node-control policy console: check, diff, apply (with confirm-or-revert) and pull the headscale
# ACL file. No credentials are used here: node-control identifies the caller by its tailnet identity, so run this from a
# tailnet node whose user is on the console's allow-list.
#
#   headscale-policy.sh check     [--file F]                validate F on the server and show its warnings
#   headscale-policy.sh diff      [--file F]                unified diff live -> F (exit 1 when they differ)
#   headscale-policy.sh apply     [--file F] [--yes] [--allow-dirty]
#                                                          read the live state, check, show the diff, apply, verify that
#                                                          node-control still answers and serves F, then confirm
#   headscale-policy.sh pull      [--file F]                write the live policy to F
#   headscale-policy.sh bootstrap [--file F] --yes          preload F into headscale's database through `fly ssh`
#                                                          (needs HS_APP), then read it back and compare
#
# bootstrap is the first step of the move to `policy.mode: database`: run it BEFORE deploying the database-mode config.
# In database mode a server with no stored policy allows everything, so the policy must already be in the database when
# the mode is switched. It writes the row directly (a copy of the config with the policy block set to database mode, so it
# works while the server still runs in file mode). It is also the break-glass path when node-control is down.
#
# Environment:
#   NODE_CONTROL_URL  base URL of node-control on the tailnet (required, except for bootstrap)
#   POLICY_FILE       policy file, default acl.json (--file overrides)
#   HS_APP            Fly app running headscale (bootstrap only)
#   RETRY_SLEEP       seconds between the post-apply access checks, default 2
#
# Exit codes: 0 ok / no diff; 1 diff present (diff); 2 usage or validation failure (also server refusals 409/413/422);
# 3 remote failure (also: bootstrap could not be verified); 4 apply was not confirmed (access lost, the live policy is not
# the applied one, or the server answered 409 to the confirmation): the server reverts it by itself at the deadline.
#
# apply verifies with at most 3 tries of `GET /policy/state`, each capped at 10 s, with RETRY_SLEEP between them: the worst
# case is 3 x 10 s + 2 x RETRY_SLEEP (34 s by default) before it gives up and exits 4. Every other request is capped at 30 s.
set -eu

MAX_BOOTSTRAP_BYTES=65536

say() { printf '%s\n' "$*" >&2; }
die() { code=$1; shift; say "headscale-policy: $*"; exit "$code"; }
usage() { say "usage: headscale-policy.sh {check|diff|apply|pull|bootstrap} [--file FILE] [--yes] [--allow-dirty]"; exit 2; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

command -v jq >/dev/null 2>&1 || die 2 "jq is required"

cmd=${1:-}
[ -n "$cmd" ] || usage
shift
case $cmd in check|diff|apply|pull|bootstrap) ;; *) usage ;; esac

file=${POLICY_FILE:-acl.json}
yes=0; allow_dirty=0
while [ $# -gt 0 ]; do
  case $1 in
    --file) [ $# -ge 2 ] || die 2 "--file needs a value"; file=$2; shift 2 ;;
    --yes) yes=1; shift ;;
    --allow-dirty) allow_dirty=1; shift ;;
    *) die 2 "unknown option: $1" ;;
  esac
done

if [ "$cmd" != bootstrap ]; then
  url=${NODE_CONTROL_URL:-}
  [ -n "$url" ] || die 2 "NODE_CONTROL_URL is required"
  case $url in -*) die 2 "NODE_CONTROL_URL must not start with '-'" ;; esac
  url=${url%/}
fi

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

# refuse: an HTTP status that is not the expected one ends the run with the matching exit code.
refuse() {
  case $code in
    409) die 2 "the policy changed or another change is pending" ;;
    413|422) _d=$(detail); die 2 "the server refused the policy (HTTP $code)${_d:+: $_d}" ;;
    403) die 3 "node-control refused this identity (HTTP 403)" ;;
    *) die 3 "unexpected answer from node-control (HTTP $code)" ;;
  esac
}

need_file() { [ -f "$file" ] || die 2 "policy file not found: $file"; }

do_check() {
  need_file
  jq -n --rawfile p "$file" '{policy:$p}' > "$tmp/body"
  remote POST /policy/preview "$tmp/body"
  [ "$code" = 200 ] || refuse
  cp "$tmp/resp" "$tmp/preview.json"
  jq -r '.warnings[]? | "warning: " + .' "$tmp/preview.json" >&2
  if [ "$(jq -r '.valid' "$tmp/preview.json")" != true ]; then
    die 2 "invalid policy: $(jq -r '.error // "no reason given"' "$tmp/preview.json")"
  fi
  say "policy is valid"
}

# live_policy: fetch the live policy text into $tmp/live (exactly, no newline added) and updated_at into $tmp/base.
live_policy() {
  remote GET /policy/state
  [ "$code" = 200 ] || refuse
  jq -e '.policy | type == "string"' "$tmp/resp" >/dev/null 2>&1 || die 3 "node-control returned no policy text"
  jq -j '.policy' "$tmp/resp" > "$tmp/live"
  jq -r '.updated_at // ""' "$tmp/resp" > "$tmp/base"
}

# norm_pair A B: write $tmp/a.n and $tmp/b.n, both through `jq -S .` when both parse as JSON, else as raw copies.
norm_pair() {
  if jq -S . "$1" > "$tmp/a.n" 2>/dev/null && jq -S . "$2" > "$tmp/b.n" 2>/dev/null; then
    :
  else
    cp "$1" "$tmp/a.n"; cp "$2" "$tmp/b.n"
  fi
}

same_text() { norm_pair "$1" "$2"; cmp -s "$tmp/a.n" "$tmp/b.n"; }

do_diff() {
  need_file
  live_policy
  norm_pair "$tmp/live" "$file"
  if diff -u -L live -L file "$tmp/a.n" "$tmp/b.n"; then
    return 0
  fi
  return 1
}

do_apply() {
  need_file
  if [ "$allow_dirty" = 0 ]; then
    _dir=$(dirname "$file")
    if git -C "$_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
      if [ -n "$(git -C "$_dir" status --porcelain -- "$(basename "$file")")" ]; then
        die 2 "$file has uncommitted changes; commit it first or pass --allow-dirty"
      fi
    fi
  fi
  # Read the base before the preview and the prompt: a change that lands while the operator reads the diff then fails
  # the apply with 409 instead of being overwritten.
  live_policy
  do_check
  _d=$(jq -r '.diff // ""' "$tmp/preview.json")
  if [ -n "$_d" ]; then printf '%s\n' "$_d"; else say "no difference from the live policy"; fi
  if [ "$yes" = 0 ]; then
    if [ -t 0 ]; then
      printf 'Apply this policy? [y/N] ' >&2
      read -r _a || _a=
      case $_a in y|Y|yes) ;; *) die 2 "not applied" ;; esac
    else
      die 2 "refusing to apply without --yes"
    fi
  fi

  jq -n --rawfile p "$file" --arg b "$(cat "$tmp/base")" '{policy:$p, base_updated_at:$b}' > "$tmp/body"
  remote POST /policy/apply "$tmp/body"
  case $code in
    200) ;;
    202)
      if [ "$(jq -r '.uncertain // false' "$tmp/resp" 2>/dev/null)" = true ]; then
        say "warning: the outcome of the change is unknown; an automatic revert is armed, verifying"
      else
        refuse
      fi ;;
    *) refuse ;;
  esac
  _cid=$(jq -r '.change_id // empty' "$tmp/resp" 2>/dev/null || true)
  case $_cid in ''|*[!0-9]*) die 3 "the answer has no change id, cannot confirm; the change reverts automatically at the deadline" ;; esac
  say "applied; verifying access under the new rules"

  _try=0; _reached=0
  while [ "$_try" -lt 3 ]; do
    _try=$((_try + 1))
    if REQ_MAX_TIME=10 req GET /policy/state && [ "$code" = 200 ]; then _reached=1; break; fi
    [ "$_try" -lt 3 ] && sleep "${RETRY_SLEEP:-2}"
  done
  if [ "$_reached" = 0 ]; then
    say "cannot reach node-control under the new rules; NOT confirming."
    say "the policy reverts automatically at the deadline"
    exit 4
  fi

  if ! jq -e '.policy | type == "string"' "$tmp/resp" >/dev/null 2>&1 \
     || ! jq -j '.policy' "$tmp/resp" > "$tmp/applied" || ! same_text "$tmp/applied" "$file"; then
    say "the live policy is not the one that was applied; it reverts automatically"
    exit 4
  fi

  jq -n --argjson id "$_cid" '{change_id:$id}' > "$tmp/cbody"
  req POST /policy/confirm "$tmp/cbody" || die 3 "could not send the confirmation; the change will revert at the deadline"
  if [ "$code" = 409 ]; then
    say "the change was not confirmed; it has been or will be reverted"
    exit 4
  fi
  [ "$code" = 204 ] || die 3 "confirmation failed (HTTP $code); the change will revert at the deadline"
  say "confirmed"
}

do_pull() {
  live_policy
  _dir=$(dirname "$file")
  _t=$(mktemp "$_dir/.$(basename "$file").XXXXXX") || die 3 "cannot create a temporary file in $_dir"
  # keep the mode of an existing file (cp -p is portable, stat is not); a new file gets 0644
  if [ -f "$file" ]; then cp -p "$file" "$_t"; else chmod 644 "$_t"; fi
  if cat "$tmp/live" > "$_t" && mv "$_t" "$file"; then
    say "wrote $file"
  else
    rm -f "$_t"
    die 3 "could not write $file"
  fi
}

# Rewrites the top-level `policy:` block of a headscale config to `policy:` + `  mode: database`; every other line is kept
# (blank lines before the next key included). Runs on the headscale machine, see do_bootstrap.
AWK_POLICY='/^policy:/ { print; print "  mode: database"; skip = 1; nb = 0; next }
skip && /^[[:space:]]+[^[:space:]]/ { nb = 0; next }
skip && /^[[:space:]]*$/ { nb++; next }
{ if (skip) for (i = 0; i < nb; i++) print ""; skip = 0; nb = 0; print }
END { if (skip) for (i = 0; i < nb; i++) print "" }'

do_bootstrap() {
  app=${HS_APP:-}
  [ -n "$app" ] || die 2 "HS_APP is required for bootstrap"
  case $app in -*) die 2 "HS_APP must not start with '-'" ;; esac
  [ "$yes" = 1 ] || die 2 "bootstrap replaces the live policy: pass --yes"
  need_file
  _size=$(wc -c < "$file" | tr -d ' ')
  [ "$_size" -le "$MAX_BOOTSTRAP_BYTES" ] || die 2 "$file is larger than $MAX_BOOTSTRAP_BYTES bytes"
  _b64=$(base64 < "$file" | tr -d '\n')
  # The remote script goes over as base64 so that no quoting survives the ssh/sh layers. It writes the policy straight
  # into headscale's database with a copy of the config whose policy block says `mode: database`; this works while the
  # server still runs in file mode. It then prints a marker and the stored policy for the local comparison.
  _rs=$(printf '%s\n' \
    'set -u' \
    'trap '"'"'rm -f /tmp/policy-bootstrap.json /tmp/hs-bootstrap-config.yaml'"'"' EXIT' \
    "echo '$_b64' | base64 -d > /tmp/policy-bootstrap.json" \
    "awk '$AWK_POLICY' /etc/headscale/config.yaml > /tmp/hs-bootstrap-config.yaml" \
    'headscale policy set -f /tmp/policy-bootstrap.json --bypass-grpc-and-access-database-directly --force -c /tmp/hs-bootstrap-config.yaml || exit $?' \
    'echo ---POLICY---' \
    'headscale policy get --bypass-grpc-and-access-database-directly -c /tmp/hs-bootstrap-config.yaml')
  _sb64=$(printf '%s\n' "$_rs" | base64 | tr -d '\n')
  flyctl ssh console -a "$app" -C "sh -c 'echo $_sb64 | base64 -d > /tmp/hs-bootstrap.sh && sh /tmp/hs-bootstrap.sh; rc=\$?; rm -f /tmp/hs-bootstrap.sh; exit \$rc'" > "$tmp/out" \
    || die 3 "flyctl ssh failed; the policy was NOT loaded"
  grep -qx -e '---POLICY---' "$tmp/out" || die 3 "no policy read back; the policy was NOT loaded"
  sed -n -e '/^---POLICY---$/,$p' "$tmp/out" | sed -e 1d > "$tmp/got"
  same_text "$tmp/got" "$file" || die 3 "the policy read back differs from $file; the policy was NOT loaded"
  say "policy preloaded into headscale's database; now switch the server to policy.mode: database"
}

case $cmd in
  check) do_check ;;
  diff) do_diff ;;
  apply) do_apply ;;
  pull) do_pull ;;
  bootstrap) do_bootstrap ;;
esac

#!/bin/sh
# CLI client of the node-control bridge selection console (#314): write the console values back to enroll.json, like
# `headscale-policy.sh pull` does for the ACL. No credentials are used here: node-control identifies the caller by its
# tailnet identity, so run this from a tailnet node whose user may view enrolment.
#
#   bridge-selection.sh diff [--file F]   unified diff F -> F with the console values (exit 1 when they differ)
#   bridge-selection.sh pull [--file F]   write the console values into F
#
# What it writes: the top-level "urltest" object (only values that are not the built-in default; no object when all
# are defaults), the "group" of each bridge (removed when empty), and the order of the "ingresses" list. Every other
# key, and every key of each bridge (host, public_key, short_id, sni, port, tokens, ...), stays as it is. A bridge that
# the console drains stays in the file: enroll.json has no drain key. The command names it, so you can remove it from
# the file yourself. After a pull and a redeploy, clear the console values (revert them) so that the file is the source.
#
# Environment:
#   NODE_CONTROL_URL  base URL of node-control on the tailnet (required)
#   ENROLL_FILE       enroll.json, default enroll.json (--file overrides)
#
# Exit codes: 0 ok / no diff; 1 diff present (diff); 2 usage or a bad file; 3 remote failure.
set -eu

say() { printf '%s\n' "$*" >&2; }
die() { code=$1; shift; say "bridge-selection: $*"; exit "$code"; }
usage() { say "usage: bridge-selection.sh {diff|pull} [--file FILE]"; exit 2; }

command -v jq >/dev/null 2>&1 || die 2 "jq is required"

cmd=${1:-}
[ -n "$cmd" ] || usage
shift
case $cmd in diff|pull) ;; *) usage ;; esac

file=${ENROLL_FILE:-enroll.json}
while [ $# -gt 0 ]; do
  case $1 in
    --file) [ $# -ge 2 ] || die 2 "--file needs a value"; file=$2; shift 2 ;;
    *) die 2 "unknown option: $1" ;;
  esac
done

url=${NODE_CONTROL_URL:-}
[ -n "$url" ] || die 2 "NODE_CONTROL_URL is required"
case $url in -*) die 2 "NODE_CONTROL_URL must not start with '-'" ;; esac
url=${url%/}

[ -f "$file" ] || die 2 "$file does not exist"
jq -e '.ingresses | type == "array"' "$file" >/dev/null 2>&1 || die 2 "$file is not an enroll.json with an \"ingresses\" list"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

code=$(curl --silent --show-error --max-time "${REQ_MAX_TIME:-30}" -H 'Accept: application/json' \
  -o "$tmp/export" -w '%{http_code}' "$url/enroll/selection/export") || die 3 "could not reach node-control"
case $code in
  200) ;;
  404) die 3 "the bridge selection console is off (ENROLL_SELECTION_UI) or enrolment is off" ;;
  401|403) die 3 "not allowed (HTTP $code): this tailnet user needs enroll view" ;;
  *) die 3 "export failed (HTTP $code)" ;;
esac
jq -e '(.urltest | type == "object") and (.order | type == "array") and (.groups | type == "object")' "$tmp/export" \
  >/dev/null 2>&1 || die 3 "the export is not in the expected shape"

# merge: the console values over the file. A bridge of the file that the console does not know keeps its place at the
# end, in file order.
jq --slurpfile ex "$tmp/export" '
  ($ex[0]) as $e
  | (if ($e.urltest | length) > 0 then .urltest = $e.urltest else del(.urltest) end)
  | .ingresses = (
      [ .ingresses[] ] as $all
      | ([ $e.order[] as $n | $all[] | select(.name == $n) ] + [ $all[] | select(.name as $n | ($e.order | index($n)) == null) ])
      | map(.name as $n
            | if ($e.groups | has($n)) then
                (if $e.groups[$n] == "" then del(.group) else .group = $e.groups[$n] end)
              else . end)
    )
' "$file" > "$tmp/new" || die 2 "could not merge into $file"

# drained bridges: say so, never remove them
jq -r '.drained[]?' "$tmp/export" | while IFS= read -r n; do
  say "note: the console drains bridge $n; it stays in $file (enroll.json has no drain key)"
done

# compare as normalised JSON, so formatting alone is no difference
jq -S . "$file" > "$tmp/old.n"
jq -S . "$tmp/new" > "$tmp/new.n"

case $cmd in
  diff)
    if diff -u "$tmp/old.n" "$tmp/new.n"; then say "no difference"; exit 0; fi
    exit 1 ;;
  pull)
    if cmp -s "$tmp/old.n" "$tmp/new.n"; then say "$file already has the console values"; exit 0; fi
    _dir=$(dirname "$file")
    _t=$(mktemp "$_dir/.$(basename "$file").XXXXXX") || die 3 "cannot create a temporary file in $_dir"
    cp -p "$file" "$_t"
    if jq . "$tmp/new" > "$_t" && mv "$_t" "$file"; then
      say "wrote $file"
    else
      rm -f "$_t"
      die 3 "could not write $file"
    fi ;;
esac

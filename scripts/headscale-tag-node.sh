#!/bin/sh
# Show, add or remove an admin-set (forced) tag on one headscale node, through the headscale REST API, without
# dropping the node's other forced tags.
#
#   headscale-tag-node.sh show   --node GIVEN_NAME                  print the node's id and its tags: forced (admin-set),
#                                                                   valid (requested by the node and allowed by the
#                                                                   policy's tagOwners) and other
#   headscale-tag-node.sh add    --node GIVEN_NAME --tag TAG [--yes]   forced := forced + TAG
#   headscale-tag-node.sh remove --node GIVEN_NAME --tag TAG [--yes]   forced := forced - TAG
#
# The node is found by its headscale GIVEN name, exactly (never the host name it reports); zero or several matches are
# refused. The API call that sets tags REPLACES the node's whole forced set, so the script sends the current forced
# tags plus or minus TAG, sorted and without duplicates, and only ever changes forced tags. Without --yes it prints the
# plan (`forced: [a b] -> [a b c]`) and exits 1, like a diff. With --yes it applies the plan, reads the node again and
# checks that the forced set is the intended one.
#
# Environment:
#   HEADSCALE_URL          headscale base URL, e.g. https://headscale.example.com (required)
#   HEADSCALE_API_KEY      API key (required unless HEADSCALE_API_KEY_CMD is set)
#   HEADSCALE_API_KEY_CMD  shell command that prints the API key (e.g. a sops or password-manager call); run with
#                          `sh -c`, its output is never echoed. It wins when both are set.
#   REQ_MAX_TIME           seconds per request, default 30
#
# The key never appears on a command line, in a file or in the output: it reaches curl as a config on stdin (-K -).
#
# Exit codes: 0 ok / nothing to change; 1 a change is planned but --yes was not given; 2 usage or validation failure
# (bad tag, no node or several nodes with that given name, unusable key); 3 remote failure (unreachable, refused,
# unexpected answer) or the node's forced tags are not the intended ones after the change.
set -eu

# headscale's SetTags RPC: POST /api/v1/node/{node_id}/tags with {"tags": [...]}; it replaces the node's forced tags.
TAGS_ROUTE_PREFIX=/api/v1/node/
TAGS_ROUTE_SUFFIX=/tags
LIST_ROUTE=/api/v1/node

say() { printf '%s\n' "$*" >&2; }
die() { code=$1; shift; say "headscale-tag-node: $*"; exit "$code"; }
usage() { say "usage: headscale-tag-node.sh {show|add|remove} --node GIVEN_NAME [--tag TAG] [--yes]"; exit 2; }

command -v jq >/dev/null 2>&1 || die 2 "jq is required"
command -v curl >/dev/null 2>&1 || die 2 "curl is required"

cmd=${1:-}
[ -n "$cmd" ] || usage
shift
case $cmd in show|add|remove) ;; *) usage ;; esac

node=; tag=; yes=0
while [ $# -gt 0 ]; do
  case $1 in
    --node) [ $# -ge 2 ] || die 2 "--node needs a value"; node=$2; shift 2 ;;
    --tag) [ $# -ge 2 ] || die 2 "--tag needs a value"; tag=$2; shift 2 ;;
    --yes) yes=1; shift ;;
    *) die 2 "unknown option: $1" ;;
  esac
done

[ -n "$node" ] || die 2 "--node is required (the node's headscale given name)"
if [ "$cmd" = show ]; then
  [ -z "$tag" ] || die 2 "show takes no --tag"
  [ "$yes" = 0 ] || die 2 "show takes no --yes"
else
  [ -n "$tag" ] || die 2 "$cmd needs --tag"
  jq -nre --arg t "$tag" '$t | test("\\Atag:[a-z0-9][a-z0-9-]{0,62}\\z")' >/dev/null 2>&1 \
    || die 2 "--tag must match ^tag:[a-z0-9][a-z0-9-]{0,62}\$"
fi

base=${HEADSCALE_URL:-}
[ -n "$base" ] || die 2 "HEADSCALE_URL is required"
case $base in -*) die 2 "HEADSCALE_URL must not start with '-'" ;; esac
case $base in http://*|https://*) ;; *) die 2 "HEADSCALE_URL must start with https:// (or http://)" ;; esac
base=${base%/}

if [ -n "${HEADSCALE_API_KEY_CMD:-}" ]; then
  key=$(sh -c "$HEADSCALE_API_KEY_CMD" 2>/dev/null) || die 2 "HEADSCALE_API_KEY_CMD failed (its output is not shown)"
else
  key=${HEADSCALE_API_KEY:-}
fi
[ -n "$key" ] || die 2 "set HEADSCALE_API_KEY or HEADSCALE_API_KEY_CMD"
# The key goes into a quoted curl config line: refuse characters that would break out of it (the key is not shown).
case $key in
  *'"'*|*'\'*|*"
"*|*' '*) die 2 "the API key contains a quote, backslash, space or newline; refusing to use it" ;;
esac

tmp=$(mktemp -d)
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

# api METHOD PATH [BODYFILE]: sets code and leaves the answer in $tmp/resp; returns 1 on a transport failure. The
# Authorization header travels in a curl config on stdin; printf is a shell builtin, so the key is in no argv.
api() {
  _m=$1; _p=$2; _b=${3:-}
  : > "$tmp/resp"
  if [ -n "$_b" ]; then
    code=$(printf 'header = "Authorization: Bearer %s"\n' "$key" \
      | curl -q --silent --show-error --connect-timeout 10 --max-time "${REQ_MAX_TIME:-30}" -K - \
        -X "$_m" -H 'Content-Type: application/json' --data-binary @"$_b" \
        -o "$tmp/resp" -w '%{http_code}' "$base$_p") || return 1
  else
    code=$(printf 'header = "Authorization: Bearer %s"\n' "$key" \
      | curl -q --silent --show-error --connect-timeout 10 --max-time "${REQ_MAX_TIME:-30}" -K - \
        -X "$_m" -o "$tmp/resp" -w '%{http_code}' "$base$_p") || return 1
  fi
}

refuse() { # WHAT
  case $code in
    401|403) die 3 "headscale refused the API key while $1 (HTTP $code)" ;;
    *) die 3 "unexpected answer from headscale while $1 (HTTP $code)" ;;
  esac
}

# Field names as node-control's headscale client reads them (camelCase and snake_case; invalid tags are ignored).
NORM='def first(a; b): if (a // "") != "" then a else (b // "") end;
  def norm: {
    id: ((.id // "") | tostring),
    given: first(.givenName; .given_name),
    forced: (((.forcedTags // []) + (.forced_tags // [])) | unique),
    valid: (((.validTags // []) + (.valid_tags // [])) | unique),
    tags: ((.tags // []) | unique),
    has_forced: (has("forcedTags") or has("forced_tags"))
  } | .other = (.tags - .forced - .valid);'

# find_node: the one node whose given name equals $node, normalised, into $tmp/node.json.
find_node() {
  api GET "$LIST_ROUTE" || die 3 "cannot reach headscale (GET $LIST_ROUTE)"
  [ "$code" = 200 ] || refuse "listing nodes"
  jq -e '.nodes | type == "array"' "$tmp/resp" >/dev/null 2>&1 || die 3 "headscale returned no node list"
  jq --arg n "$node" "$NORM [.nodes[] | norm | select(.given == \$n)]" "$tmp/resp" > "$tmp/matches.json" \
    || die 3 "cannot read headscale's node list"
  _count=$(jq 'length' "$tmp/matches.json")
  case $_count in
    1) jq '.[0]' "$tmp/matches.json" > "$tmp/node.json" ;;
    0) die 2 "no node has the given name '$node' (the given name, not the host name; see 'headscale nodes list')" ;;
    *) die 2 "$_count nodes have the given name '$node' (ids $(jq -r 'map(.id) | join(", ")' "$tmp/matches.json")); rename one first" ;;
  esac
}

list() { jq -r --arg k "$1" '"[" + (.[$k] | join(" ")) + "]"' "$tmp/node.json"; }

show_node() {
  printf 'node: %s (id %s)\n' "$node" "$(jq -r '.id' "$tmp/node.json")"
  printf 'forced (admin-set): %s\n' "$(list forced)"
  printf 'valid (requested by the node, allowed by tagOwners): %s\n' "$(list valid)"
  printf 'other: %s\n' "$(list other)"
}

find_node
id=$(jq -r '.id' "$tmp/node.json")
case $id in ''|*[!0-9]*) die 3 "headscale returned no usable id for '$node'" ;; esac

if [ "$cmd" = show ]; then
  show_node
  exit 0
fi

# A headscale that reports tags only in one list cannot tell admin-set tags from requested ones: replacing "forced"
# there could drop tags, so refuse instead of guessing.
if [ "$(jq -r '.has_forced or (.other | length == 0)' "$tmp/node.json")" != true ]; then
  show_node
  die 2 "this headscale reports the node's tags without a forced-tags field, so the current admin-set tags are unknown; refusing to replace them (use the headscale CLI)"
fi

if [ "$cmd" = add ]; then
  jq --arg t "$tag" '.forced + [$t] | unique' "$tmp/node.json" > "$tmp/want.json"
else
  jq --arg t "$tag" '.forced - [$t] | unique' "$tmp/node.json" > "$tmp/want.json"
fi
cur=$(list forced)
want=$(jq -r '"[" + join(" ") + "]"' "$tmp/want.json")

show_node
if [ "$cmd" = remove ] && jq -e --arg t "$tag" '.valid | index($t)' "$tmp/node.json" >/dev/null 2>&1; then
  say "note: $tag is also a tag the node requests itself; removing it from the forced set does not remove that (change the node's --advertise-tags or the policy's tagOwners)"
fi
if [ "$cur" = "$want" ]; then
  printf 'forced: %s (no change)\n' "$cur"
  exit 0
fi
printf 'forced: %s -> %s\n' "$cur" "$want"
printf 'note: the API call REPLACES the node'"'"'s forced tags, so the whole new set above is sent; valid and other tags are not touched\n'
[ "$yes" = 1 ] || { say "not applied: rerun with --yes to apply"; exit 1; }

jq -n --slurpfile w "$tmp/want.json" '{tags: $w[0]}' > "$tmp/body"
route=$TAGS_ROUTE_PREFIX$id$TAGS_ROUTE_SUFFIX
api POST "$route" "$tmp/body" || die 3 "cannot reach headscale (POST $route); the tags may or may not have changed: run show"
[ "$code" = 200 ] || refuse "setting the tags (the tags may or may not have changed: run show)"

find_node
if [ "$(jq -r '.id' "$tmp/node.json")" != "$id" ] \
   || [ "$(jq -c '.forced' "$tmp/node.json")" != "$(jq -c '.' "$tmp/want.json")" ]; then
  show_node
  die 3 "after the change the node's forced tags are $(list forced), not $want"
fi
printf 'applied: forced tags of %s are now %s\n' "$node" "$want"

#!/bin/sh
# Show, add or remove an admin-held tag on one headscale node, through the headscale REST API, without dropping the
# node's other admin-held tags. Two tag models are supported and the one in use is printed:
#
#   unified  headscale 0.28 and later: a node has ONE `tags` list. Tags are taken from --advertise-tags at registration
#            (checked against the policy's tagOwners) and afterwards change only through an admin (`headscale nodes
#            tag` or this API). Tags and user ownership exclude each other: tagging a user-owned (tag-less) node
#            removes its user ownership, which only re-authenticating with empty tags undoes.
#   legacy   older headscale: `forcedTags` (admin-set), `validTags` (requested by the node, allowed by tagOwners) and
#            `invalidTags` (ignored). Only the forced set is changed.
#
#   headscale-tag-node.sh show   --node GIVEN_NAME                  print the node's id, the model and its tags
#   headscale-tag-node.sh add    --node GIVEN_NAME --tag TAG [--yes] [--allow-ownership-change]   set := set + TAG
#   headscale-tag-node.sh remove --node GIVEN_NAME --tag TAG [--yes] [--allow-ownership-change]   set := set - TAG
#
# "set" is `tags` (unified) or the forced tags (legacy). The node is found by its headscale GIVEN name, exactly (never
# the host name it reports); zero or several matches are refused. The API call that sets tags REPLACES the whole set,
# so the script sends the current set plus or minus TAG, sorted and without duplicates. Without --yes it prints the
# plan (`tags: [a b] -> [a b c]`) and exits 1, like a diff. With --yes it applies the plan, reads the node again and
# checks that the set is the intended one. In the unified model, adding the first tag to a node (it loses its user
# ownership) and removing its last tag also need --allow-ownership-change.
#
# Environment:
#   HEADSCALE_URL          headscale base URL, e.g. https://headscale.example.com (required)
#   HEADSCALE_API_KEY      API key (required unless HEADSCALE_API_KEY_CMD is set)
#   HEADSCALE_API_KEY_CMD  shell command that prints the API key (e.g. a sops or password-manager call); run with
#                          `sh -c`, its output is never echoed. It wins when both are set.
#   REQ_MAX_TIME           seconds per request, default 30
#
# The key never appears on a command line, in a file or in the output (xtrace is disabled in the script): it reaches
# curl as a config on stdin (-K -).
#
# Exit codes: 0 ok / nothing to change; 1 a change is planned but --yes (or --allow-ownership-change, for a first tag)
# was not given; 2 usage or validation failure (bad tag, no node or several nodes with that given name, unusable key,
# unrecognised node shape, an ownership change without --allow-ownership-change); 3 remote failure (unreachable,
# refused, unexpected answer) or the node's tags are not the intended ones after the change.
set -eu
set +x

# headscale's SetTags RPC: POST /api/v1/node/{node_id}/tags with {"tags": [...]}; it replaces the node's whole tag set
# (`tags` on 0.28 and later, the forced tags before).
TAGS_ROUTE_PREFIX=/api/v1/node/
TAGS_ROUTE_SUFFIX=/tags
LIST_ROUTE=/api/v1/node

say() { printf '%s\n' "$*" >&2; }
die() { code=$1; shift; say "headscale-tag-node: $*"; exit "$code"; }
usage() { say "usage: headscale-tag-node.sh {show|add|remove} --node GIVEN_NAME [--tag TAG] [--yes] [--allow-ownership-change]"; exit 2; }

command -v jq >/dev/null 2>&1 || die 2 "jq is required"
command -v curl >/dev/null 2>&1 || die 2 "curl is required"

cmd=${1:-}
[ -n "$cmd" ] || usage
shift
case $cmd in show|add|remove) ;; *) usage ;; esac

node=; tag=; yes=0; allow_own=0
while [ $# -gt 0 ]; do
  case $1 in
    --node) [ $# -ge 2 ] || die 2 "--node needs a value"; node=$2; shift 2 ;;
    --tag) [ $# -ge 2 ] || die 2 "--tag needs a value"; tag=$2; shift 2 ;;
    --yes) yes=1; shift ;;
    --allow-ownership-change) allow_own=1; shift ;;
    *) die 2 "unknown option: $1" ;;
  esac
done

[ -n "$node" ] || die 2 "--node is required (the node's headscale given name)"
if [ "$cmd" = show ]; then
  [ -z "$tag" ] || die 2 "show takes no --tag"
  [ "$yes" = 0 ] || die 2 "show takes no --yes"
  [ "$allow_own" = 0 ] || die 2 "show takes no --allow-ownership-change"
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
  cr=$(printf '\r')
  key=${key%"$cr"}
else
  key=${HEADSCALE_API_KEY:-}
fi
[ -n "$key" ] || die 2 "set HEADSCALE_API_KEY or HEADSCALE_API_KEY_CMD"
# The key goes into a quoted curl config line: accept only the characters API keys use (the key is not shown).
case $key in
  *[!A-Za-z0-9._~+/=-]*) die 2 "unusable API key: it may only contain letters, digits and . _ ~ + / = - (not shown)" ;;
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
# model: legacy when a forced-tags field exists, unified when only `tags` exists, unknown otherwise. "set" is what the
# tags call replaces.
NORM='def first(a; b): if (a // "") != "" then a else (b // "") end;
  def norm: {
    id: ((.id // "") | tostring),
    given: first(.givenName; .given_name),
    forced: (((.forcedTags // []) + (.forced_tags // [])) | unique),
    valid: (((.validTags // []) + (.valid_tags // [])) | unique),
    tags: ((.tags // []) | unique),
    model: (if has("forcedTags") or has("forced_tags") then "legacy" elif has("tags") then "unified" else "unknown" end)
  } | .other = (.tags - .forced - .valid)
    | .set = (if .model == "legacy" then .forced else .tags end);'

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
field() { jq -r --arg k "$1" '.[$k]' "$tmp/node.json"; }

show_node() {
  printf 'node: %s (id %s)\n' "$node" "$(field id)"
  case $(field model) in
    unified)
      printf 'model: unified (headscale 0.28+: one tags list; tagging a user-owned node removes its user ownership)\n'
      printf 'tags (admin-held after registration): %s\n' "$(list tags)" ;;
    legacy)
      printf 'model: legacy (forced and valid tags; only the forced tags are changed)\n'
      printf 'forced (admin-set): %s\n' "$(list forced)"
      printf 'valid (requested by the node, allowed by tagOwners): %s\n' "$(list valid)"
      printf 'other: %s\n' "$(list other)" ;;
  esac
}

find_node
id=$(field id)
case $id in ''|*[!0-9]*) die 3 "headscale returned no usable id for '$node'" ;; esac
model=$(field model)
[ "$model" != unknown ] || die 2 "unrecognised node shape: the node has neither a forced-tags field nor a tags field; refusing (use the headscale CLI)"
if [ "$model" = legacy ]; then label=forced; else label=tags; fi

if [ "$cmd" = show ]; then
  show_node
  exit 0
fi

if [ "$cmd" = add ]; then
  jq --arg t "$tag" '.set + [$t] | unique' "$tmp/node.json" > "$tmp/want.json"
else
  jq --arg t "$tag" '.set - [$t] | unique' "$tmp/node.json" > "$tmp/want.json"
fi
cur=$(list set)
want=$(jq -r '"[" + join(" ") + "]"' "$tmp/want.json")

show_node
if [ "$model" = legacy ] && jq -e --arg t "$tag" '(.valid | index($t)) and ((.forced | index($t)) | not)' "$tmp/node.json" >/dev/null 2>&1; then
  if [ "$cmd" = add ]; then
    say "note: $tag is now a tag the node requests itself (valid); adding it makes it admin-held (forced) as well"
  else
    say "note: $tag is also a tag the node requests itself; removing it from the forced set does not remove that (change the node's --advertise-tags or the policy's tagOwners)"
  fi
fi
if [ "$cur" = "$want" ]; then
  printf '%s: %s (no change)\n' "$label" "$cur"
  exit 0
fi

# Unified model: tags and user ownership exclude each other.
own_change=
if [ "$model" = unified ] && [ "$cur" = "[]" ]; then
  own_change=first
elif [ "$model" = unified ] && [ "$want" = "[]" ]; then
  own_change=last
fi
if [ "$own_change" = last ] && [ "$allow_own" = 0 ]; then
  die 2 "removing the last tag would leave the node with no tags: on headscale 0.28 and later a tagged node has no user owner, so a tag-less node needs re-authentication and the server may reject this; pass --allow-ownership-change to try anyway"
fi

printf '%s: %s -> %s\n' "$label" "$cur" "$want"
if [ "$model" = legacy ]; then
  printf 'note: the API call REPLACES the node'"'"'s forced tags, so the whole new set above is sent; valid and other tags are not touched\n'
else
  printf 'note: the API call REPLACES the node'"'"'s tags, so the whole new set above is sent\n'
fi
if [ "$own_change" = first ]; then
  printf 'warning: the node has no tags, so it is owned by its user; on headscale 0.28 and later tagging it REMOVES that user ownership, and only re-authenticating the node with empty tags undoes it. Applying needs --yes and --allow-ownership-change\n'
fi
if [ "$yes" = 0 ]; then
  say "not applied: rerun with --yes${own_change:+ and --allow-ownership-change} to apply"
  exit 1
fi
if [ "$own_change" = first ] && [ "$allow_own" = 0 ]; then
  die 2 "not applied: tagging this user-owned node removes its user ownership (headscale 0.28+); pass --allow-ownership-change as well as --yes"
fi

jq -n --slurpfile w "$tmp/want.json" '{tags: $w[0]}' > "$tmp/body"
route=$TAGS_ROUTE_PREFIX$id$TAGS_ROUTE_SUFFIX
api POST "$route" "$tmp/body" || die 3 "cannot reach headscale (POST $route); the tags may or may not have changed: run show"
[ "$code" = 200 ] || refuse "setting the tags (the tags may or may not have changed: run show)"

find_node
if [ "$(field id)" != "$id" ] || [ "$(field model)" != "$model" ] \
   || [ "$(jq -c '.set' "$tmp/node.json")" != "$(jq -c '.' "$tmp/want.json")" ]; then
  show_node
  die 3 "after the change the node's $label set is $(list set), not $want"
fi
printf 'applied: %s of %s are now %s\n' "$label" "$node" "$want"

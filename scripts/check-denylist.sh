#!/usr/bin/env bash
# Refuse text that names real-world places, networks or authorities (CLAUDE.md, "No geographic or
# political references"). The pattern list is deliberately NOT in this repository: a list of
# place names in a public repo would defeat the rule. It is read from the operator's machine:
#
#   $HEADLAMP_DENYLIST, else ~/.config/headlamp/denylist
#
# one extended regular expression per line, matched case-insensitively; blank lines and lines
# starting with # are ignored. Never commit that file, paste it into an issue/PR, or quote it.
#
# usage:
#   check-denylist.sh --staged            added lines and paths of the staged changes
#   check-denylist.sh --message FILE      a commit message file
#   check-denylist.sh --text FILE|-       any text: PR title/body, issue text, branch name, ...
#
# Exit 0 = clean (or no list available and HEADLAMP_DENYLIST_REQUIRED is not 1), 1 = hit, 2 = no list
# while HEADLAMP_DENYLIST_REQUIRED=1 (agents must set it), 64 = bad usage.
set -u

usage() { echo "usage: $0 --staged | --message FILE | --text FILE|-" >&2; exit 64; }
[ $# -ge 1 ] || usage

list="${HEADLAMP_DENYLIST:-${HOME:-}/.config/headlamp/denylist}"
if [ ! -r "$list" ]; then
  echo "check-denylist: no pattern list at $list (set HEADLAMP_DENYLIST); the geography check did NOT run" >&2
  [ "${HEADLAMP_DENYLIST_REQUIRED:-0}" = 1 ] && exit 2
  exit 0
fi

pats="$(mktemp "${TMPDIR:-/tmp}/denylist.XXXXXX")" || exit 64
trap 'rm -f "$pats"' EXIT
grep -vE '^[[:space:]]*(#|$)' "$list" > "$pats" || true
[ -s "$pats" ] || { echo "check-denylist: the pattern list is empty; nothing checked" >&2; exit 0; }

case "$1" in
  --staged)
    what="staged changes"
    text="$({ git diff --cached --name-only --diff-filter=ACMR; git diff --cached -U0 --diff-filter=ACMR | grep -E '^\+' | grep -vE '^\+\+\+'; } 2>/dev/null)" ;;
  --message)
    [ $# -ge 2 ] || usage
    what="commit message"; text="$(cat "$2")" ;;
  --text)
    [ $# -ge 2 ] || usage
    what="text"; if [ "$2" = "-" ]; then text="$(cat)"; else text="$(cat "$2")"; fi ;;
  *) usage ;;
esac

hits="$(printf '%s\n' "$text" | grep -inE -f "$pats" || true)"
if [ -n "$hits" ]; then
  echo "check-denylist: the $what contains a geographic/political reference (see CLAUDE.md):" >&2
  printf '%s\n' "$hits" | head -20 | cut -c1-200 | sed 's/^/  /' >&2
  echo "check-denylist: rewrite it generically (describe the mechanism and a generic purpose); do not swap in a synonym." >&2
  exit 1
fi
exit 0

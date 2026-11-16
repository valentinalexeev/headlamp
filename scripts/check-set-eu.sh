#!/usr/bin/env bash
# ADR 0002 rule 8 (#368): every shell script stops on an error and on an unset variable. A POSIX script has
# `set -eu`; a bash script has `set -euo pipefail` (or the same flags in another order). The check reads the
# tracked *.sh files. scripts/check-set-eu.allow lists the hand-written test harnesses that #382 moves to bats-core
# and Go (ADR 0004); the list only shrinks. A listed file that has the flags is an error too, so the list stays
# current.
# Usage: scripts/check-set-eu.sh [repo dir]. Exit 0 = clean, 1 = a finding.
set -euo pipefail
cd "${1:-$(git rev-parse --show-toplevel)}"
allow=scripts/check-set-eu.allow
fail=0

listed() { grep -qxF "$1" "$allow" 2>/dev/null; }
ok_flags() { # ok_flags <file> <bash|sh>: the first `set -<letters>` line has e and u (bash: and -o pipefail)
  local line letters
  line=$(grep -E '^[[:space:]]*set -[a-z]+' "$1" | head -1 || true)
  letters=$(printf '%s\n' "$line" | sed -E 's/^[[:space:]]*set -([a-z]+).*/\1/')
  case "$letters" in *e*) ;; *) return 1 ;; esac
  case "$letters" in *u*) ;; *) return 1 ;; esac
  if [ "$2" = bash ]; then
    case "$line" in *"o pipefail"*) ;; *) grep -qE '^[[:space:]]*set -o pipefail' "$1" || return 1 ;; esac
  fi
  return 0
}

while IFS= read -r f; do
  case "$f" in test/bats/*) continue ;; esac
  kind="sh"
  if head -1 "$f" | grep -q bash; then kind=bash; fi
  if ok_flags "$f" "$kind"; then
    if listed "$f"; then echo "check-set-eu: $f has the flags now: remove it from $allow"; fail=1; fi
  elif ! listed "$f"; then
    if [ "$kind" = bash ]; then want="set -euo pipefail"; else want="set -eu"; fi
    echo "check-set-eu: $f has no '$want' (ADR 0002 rule 8)"
    fail=1
  fi
done < <(git ls-files '*.sh')

while IFS= read -r f; do
  case "$f" in ''|'#'*) continue ;; esac
  [ -f "$f" ] || { echo "check-set-eu: $f is in $allow but does not exist"; fail=1; }
done < "$allow"
exit "$fail"

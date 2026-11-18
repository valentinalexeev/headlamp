#!/usr/bin/env bash
# Changelog fragments (#455). Usage:
#   scripts/changelog.sh check                  validate every changelog.d/ fragment
#   scripts/changelog.sh preview                print the next release (CHANGELOG.md and UPGRADING.md parts)
#   scripts/changelog.sh release VERSION DATE   release PR only: write the sections, delete the released fragments
#   scripts/changelog.sh forward VERSION [REF]  main, after a patch from maint/X.Y: copy its sections, drop its fragments
#   scripts/changelog.sh pr-gate BASE [HEAD]    CI: a fragment for code changes, no direct Unreleased edits
# The format is in changelog.d/README.md. The logic is in scripts/changelog.py (Python 3 standard library only).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
py() { python3 "$here/changelog.py" "$@"; }

case "${1:-}" in
check | preview | release | forward | unreleased) py "$@" ;;
pr-gate)
	# Env: LABELS (comma-separated PR labels), HEAD_REF (PR branch name). Exit 1 on a violation.
	base="${2:?usage: pr-gate BASE [HEAD]}" head="${3:-HEAD}"
	head_ref="${HEAD_REF:-}" labels=",${LABELS:-},"
	files=$(git diff --name-only "$base"..."$head")
	rc=0
	case "$head_ref" in
	release/* | forward/*)
		echo "release or forward branch '$head_ref': CHANGELOG.md and UPGRADING.md edits allowed, no fragment needed"
		exit 0
		;;
	esac
	mb=$(git merge-base "$base" "$head")
	for f in CHANGELOG.md UPGRADING.md; do
		printf '%s\n' "$files" | grep -qx "$f" || continue
		if ! diff -q <(git show "$mb:$f" | py unreleased -) <(git show "$head:$f" | py unreleased -) >/dev/null ||
			[ "$(git show "$mb:$f" | grep -c '^## ')" != "$(git show "$head:$f" | grep -c '^## ')" ]; then
			echo "::error file=$f::this PR adds or edits an unreleased part of $f. Add a changelog.d/<issue>-<slug>.md fragment instead (changelog.d/README.md)."
			rc=1
		fi
	done
	# docs, tests, CI and repo meta need no fragment; everything else is code
	code=$(printf '%s\n' "$files" | grep -vE '^(docs/|changelog\.d/|\.github/|scripts/test/|test/|tests/|LICENSE|.*\.md$)' || true)
	frag=$(printf '%s\n' "$files" | grep -E '^changelog\.d/[0-9]+-[^/]+\.md$' || true)
	if [ -n "$code" ] && [ -z "$frag" ]; then
		if [[ "$labels" == *",no-changelog,"* ]]; then
			echo "code changed without a fragment, allowed by the 'no-changelog' label"
		else
			echo "::error::this PR changes code but adds or changes no changelog.d/<issue>-<slug>.md fragment. Add one (changelog.d/_template.md) or ask for the 'no-changelog' label. Code files:"
			printf '%s\n' "$code" | sed 's/^/  /'
			rc=1
		fi
	fi
	[ "$rc" = 0 ] && echo "ok: changelog gate"
	exit "$rc"
	;;
*)
	sed -n '2,7p' "$0" >&2
	exit 64
	;;
esac

#!/usr/bin/env bash
# Checks that a tag is a release VERSIONING.md allows. Exits non-zero, with the reason, if it is not.
#
#   check-release.sh vX.Y.Z[-rc.N] [MAIN_REF]
#
# The tag must be strict semver, match the VERSION file at that commit, be on MAIN_REF (default origin/main), be the next
# version after the highest stable tag, and bump enough for the commits since then. With RELEASE_REPO=owner/name it
# must also have no open issues in a milestone named after its version.
set -euo pipefail

fail() { echo "release check failed: $*" >&2; exit 1; }

tag=${1:?usage: check-release.sh vX.Y.Z[-rc.N] [MAIN_REF]}
main_ref=${2:-origin/main}
stable='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
pre='^(v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))-rc\.([1-9][0-9]*)$'

core=$tag
if [[ $tag =~ $pre ]]; then
	core=${BASH_REMATCH[1]}
elif [[ ! $tag =~ $stable ]]; then
	fail "$tag is not vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N"
fi
[[ $core =~ $stable ]]
major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}

commit=$(git rev-parse --verify "$tag^{commit}" 2>/dev/null) || fail "no such tag or commit: $tag"
file=$(git show "$commit:VERSION" 2>/dev/null | tr -d '[:space:]') || fail "no VERSION file at $tag"
[ "$file" = "${tag#v}" ] || [ "$file" = "${core#v}" ] || fail "VERSION says $file, the tag says ${tag#v}"
git merge-base --is-ancestor "$commit" "$main_ref" || fail "$tag is not on $main_ref"

previous=$(git tag -l 'v*' | grep -E "$stable" | grep -vx "$tag" | sort -V | tail -1 || true)

if [ -n "$previous" ]; then
	[[ $previous =~ $stable ]]
	pmajor=${BASH_REMATCH[1]} pminor=${BASH_REMATCH[2]} ppatch=${BASH_REMATCH[3]}

	# Only ever the next version, with the lower numbers reset. A fix to an older line is not supported.
	if [ "$major" = "$((pmajor + 1))" ] && [ "$minor" = 0 ] && [ "$patch" = 0 ]; then
		bump="major"
	elif [ "$major" = "$pmajor" ] && [ "$minor" = "$((pminor + 1))" ] && [ "$patch" = 0 ]; then
		bump="minor"
	elif [ "$major" = "$pmajor" ] && [ "$minor" = "$pminor" ] && [ "$patch" = "$((ppatch + 1))" ]; then
		bump="patch"
	else
		fail "$core does not follow $previous: expected the next major, minor or patch"
	fi

	need="patch"
	log=$(git log --no-merges --format='%s%n%b%n%x00' "$previous..$commit")
	if grep -qE '^[a-z]+(\([^)]*\))?!: |^BREAKING[ -]CHANGE: ' <<<"$log"; then
		need="major"
		[ "$pmajor" != 0 ] || need=minor # before 1.0.0, a breaking change moves the minor
	elif grep -qE '^feat(\([^)]*\))?: ' <<<"$log"; then
		need="minor"
	fi
	rank() { case $1 in patch) echo 0 ;; minor) echo 1 ;; major) echo 2 ;; esac; }
	[ "$(rank "$bump")" -ge "$(rank "$need")" ] ||
		fail "the commits since $previous need at least a $need release, this is a $bump"
fi

if [ -n "${RELEASE_REPO:-}" ]; then
	open=$(gh api "repos/$RELEASE_REPO/milestones?state=all&per_page=100" \
		--jq ".[] | select(.title == \"${core#v}\") | .open_issues")
	[ "${open:-0}" = 0 ] || fail "milestone ${core#v} still has $open open issues"
fi
echo "$tag is a valid release"

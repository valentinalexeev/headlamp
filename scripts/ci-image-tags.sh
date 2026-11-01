#!/usr/bin/env bash
# Prints the image tags a ref should carry, one per line.
#
#   ci-image-tags.sh branch SHA    main, sha-<12 characters>
#   ci-image-tags.sh tag vX.Y.Z    X.Y.Z, and the floating tags it is now the highest of
#
# A prerelease gets only its own tag. Floating tags (X.Y, X, latest) move only to the highest stable version, so a fix
# to an older line never moves them backwards. Before 1.0.0 there is no X or latest: 0.y gives no such promise.
set -euo pipefail

stable='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

highest() { grep -E "$stable" | sort -V | tail -1; }

case "${1:-}" in
branch)
	sha=${2:?commit}
	echo main
	echo "sha-${sha:0:12}"
	;;
tag)
	tag=${2:?tag}
	echo "${tag#v}"
	if [[ ! $tag =~ $stable ]]; then
		exit 0
	fi
	major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]}
	tags=$( (git tag -l 'v*'; echo "$tag") | sort -u)
	[ "$(grep -E "^v$major\.$minor\." <<<"$tags" | highest)" != "$tag" ] || echo "$major.$minor"
	if [ "$major" != 0 ]; then
		[ "$(grep -E "^v$major\." <<<"$tags" | highest)" != "$tag" ] || echo "$major"
		[ "$(highest <<<"$tags")" != "$tag" ] || echo latest
	fi
	;;
*)
	echo "usage: $0 branch SHA | tag vX.Y.Z" >&2
	exit 2
	;;
esac

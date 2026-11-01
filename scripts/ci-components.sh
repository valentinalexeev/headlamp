#!/usr/bin/env bash
# Lists the image components of .github/components.json as a JSON array for a CI matrix.
#
#   ci-components.sh list [REV]            every component at REV (default HEAD), with its source hash
#   ci-components.sh changed BASE [HEAD]   the components whose source differs between BASE and HEAD
#
# The source hash covers the component's manifest entry and the git trees of its paths, so it names exactly what an
# image was built from: the workflow tags each image src-<hash> and builds only when that tag is missing.
set -euo pipefail

manifest=.github/components.json
# Changing how images are built can change any of them, so it counts as a change to all.
build_paths=(.github/workflows/images.yml scripts/ci-components.sh scripts/ci-image-tags.sh)

entry_at() { # REV IMAGE: the manifest entry of IMAGE at REV, empty if there is none
	git show "$1:$manifest" 2>/dev/null | jq -c --arg i "$2" '.[] | select(.image == $i)' 2>/dev/null || true
}

hash_at() { # REV IMAGE PATHS...: nothing is hashed for a component that does not exist at REV
	local rev=$1 image=$2 entry
	shift 2
	entry=$(entry_at "$rev" "$image")
	{ printf '%s\n' "$entry"; [ -z "$entry" ] || git ls-tree -r "$rev" -- "$@"; } | git hash-object --stdin | cut -c1-16
}

describe() { # REV: every component, with its hash and build arguments
	local rev=$1 entry image args hash file key
	local -a paths
	git show "$rev:$manifest" | jq -c '.[]' | while IFS= read -r entry; do
		image=$(jq -r .image <<<"$entry")
		mapfile -t paths < <(jq -r '.paths[]' <<<"$entry")
		args=$(jq -r '(.args // {}) | to_entries[] | "\(.key)=\(.value)"' <<<"$entry")
		while IFS= read -r key; do
			[ -n "$key" ] || continue
			file=$(jq -r --arg k "$key" '.args_from_files[$k]' <<<"$entry")
			args+="${args:+$'\n'}$key=$(git show "$rev:$file" | tr -d '[:space:]')"
		done < <(jq -r '(.args_from_files // {}) | keys[]' <<<"$entry")
		hash=$(hash_at "$rev" "$image" "${paths[@]}")
		jq -c --arg h "$hash" --arg a "$args" \
			'{image, context, dockerfile, go: (.go // false), test: (.test // ""), hash: $h, build_args: $a}' <<<"$entry"
	done | jq -cs .
}

case "${1:-}" in
list)
	describe "${2:-HEAD}"
	;;
changed)
	base=${2:?base revision}
	head=${3:-HEAD}
	if ! git diff --quiet "$base" "$head" -- "${build_paths[@]}"; then
		describe "$head"
		exit 0
	fi
	all=$(describe "$head")
	out='[]'
	while IFS= read -r image; do
		mapfile -t paths < <(git show "$head:$manifest" | jq -r --arg i "$image" '.[] | select(.image == $i) | .paths[]')
		if [ "$(hash_at "$base" "$image" "${paths[@]}")" != "$(hash_at "$head" "$image" "${paths[@]}")" ]; then
			out=$(jq -c --argjson all "$all" --arg i "$image" '. + [$all[] | select(.image == $i)]' <<<"$out")
		fi
	done < <(jq -r '.[].image' <<<"$all")
	printf '%s\n' "$out"
	;;
*)
	echo "usage: $0 list [REV] | changed BASE [HEAD]" >&2
	exit 2
	;;
esac

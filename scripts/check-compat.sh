#!/usr/bin/env bash
# Checks the compatibility matrix of node-control (#300) against a change.
#
#   scripts/check-compat.sh [BASE [HEAD]]   BASE default origin/main, HEAD default HEAD
#
# Each interface in apps/node-control/internal/compat/compat.json lists the files that define it ("covers"). When a
# change touches one of them, it must also change compat.json (raise the interface version, or move its accepted
# range), because a part on the other side may now speak something else. A change that keeps the interface as it was
# says so with a line "compat: unchanged <interface>" in one of its commit messages.
#
# It also checks that every "covers" file exists. Exit 0: fine. Exit 1: the reason is printed.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
matrix=apps/node-control/internal/compat/compat.json
base=${1:-origin/main}
head=${2:-HEAD}
command -v jq >/dev/null 2>&1 || { echo "jq is needed" >&2; exit 2; }

fail=0
while IFS=$'\t' read -r iface file; do
	[ -e "$file" ] || { echo "compat.json: $iface covers $file, which does not exist"; fail=1; }
done < <(jq -r '.interfaces | to_entries[] | .key as $k | .value.covers[] | [$k, .] | @tsv' "$matrix")

if ! git rev-parse -q --verify "$base^{commit}" >/dev/null; then
	echo "base $base not found: only the file check ran"
	exit "$fail"
fi
changed=$(git diff --name-only "$base"..."$head")
messages=$(git log --format=%B "$base".."$head")
if grep -qxF "$matrix" <<<"$changed"; then
	[ "$fail" = 0 ] && echo "compat.json changed with the interfaces: ok"
	exit "$fail"
fi
while IFS=$'\t' read -r iface file; do
	grep -qxF "$file" <<<"$changed" || continue
	if grep -qE "^compat: unchanged ($iface|all)\b" <<<"$messages"; then
		echo "$file changed; $iface declared unchanged in a commit message"
		continue
	fi
	echo "$file defines the $iface interface and changed, but $matrix did not."
	echo "  Raise its version and accepted range there, or add the line 'compat: unchanged $iface' to a commit message."
	fail=1
done < <(jq -r '.interfaces | to_entries[] | .key as $k | .value.covers[] | [$k, .] | @tsv' "$matrix")
[ "$fail" = 0 ] && echo "compatibility matrix: ok"
exit "$fail"

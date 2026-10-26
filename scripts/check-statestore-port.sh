#!/bin/sh
# Shows how funnel-ingress's copy of the Redis state store drifted from node-control's original.
#
# internal/scrub and internal/statestore of apps/fly/funnel-ingress are a port of the same packages in
# apps/fly/node-control (two Go modules, two Docker contexts; extracting a shared module is a later cleanup).
# This script applies the documented substitutions to the original and diffs the result against the copy,
# so a fix made on one side is easy to carry to the other. Advisory: it always exits 0.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
src=$root/apps/fly/node-control
dst=$root/apps/fly/funnel-ingress

# Files adapted by hand beyond the substitutions (they take a plain getenv function instead of core.Env,
# and tests that scan the sources look in the other command directory): reviewed, not diffed.
by_hand="importer.go importer_test.go env_test.go redis.go"

norm() {
	sed -e 's#example.com/node-control/internal/scrub#funnel-ingress/internal/scrub#' \
	    -e 's/NODE_CONTROL_STATE_KEY/FUNNEL_STATE_KEY/g' \
	    -e 's/_node-control-import/_funnel-ingress-import/' \
	    -e 's/node-control:tsnet/funnel-ingress:tsnet/g' \
	    -e 's/TAILSCALE_AUTH_KEY/TS_AUTHKEY/g' "$1"
}

status=0
for pkg in scrub statestore; do
	for f in "$src/internal/$pkg"/*.go; do
		name=$(basename "$f")
		other=$dst/internal/$pkg/$name
		case " $by_hand " in *" $name "*) echo "adapted by hand, review: $pkg/$name"; continue ;; esac
		if [ ! -f "$other" ]; then
			echo "missing in funnel-ingress: $pkg/$name"
			status=1
			continue
		fi
		if ! norm "$f" | diff -u --label "node-control/$pkg/$name" --label "funnel-ingress/$pkg/$name" - "$other"; then
			status=1
		fi
	done
	for f in "$dst/internal/$pkg"/*.go; do
		[ -f "$src/internal/$pkg/$(basename "$f")" ] || { echo "only in funnel-ingress: $pkg/$(basename "$f")"; }
	done
done
[ "$status" -eq 0 ] && echo "state store port: no unexpected differences"
exit 0

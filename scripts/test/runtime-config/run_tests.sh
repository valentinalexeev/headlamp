#!/usr/bin/env bash
# Checks that the images take site config mounted at runtime, so a deployment can run the published images as they are
# (#97). For each image it checks two things: the env override reads the mounted file, and without the override the
# baked placeholder is still used (the overlay procedure keeps working).
# usage: run_tests.sh [headscale|exit-node|all]      needs docker; builds the images from this checkout
set -u
here="$(cd "$(dirname "$0")" && pwd)"; root="$here/../../.."
which="${1:-all}"
fails=0
ok()   { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
chmod 755 "$tmp"

headscale_tests() {
	local img=headlamp-test/headscale
	docker build -q -t "$img" "$root/apps/fly/headscale" >/dev/null || { fail "headscale: image builds"; return; }
	# A mounted config that differs from the placeholder in a way configtest reports: its own policy path.
	sed -e 's#/etc/headscale/acl.json#/config/policy.json#' -e 's#/data/#/tmp/#g' \
		"$root/apps/fly/headscale/headscale.yaml" >"$tmp/headscale.yaml"
	printf '{"tagOwners": {"tag:example": ["admin@"]}}\n' >"$tmp/policy.json"
	chmod 644 "$tmp"/*

	if docker run --rm -v "$tmp:/config:ro" -e HEADSCALE_CONFIG=/config/headscale.yaml \
		--entrypoint headscale "$img" configtest >"$tmp/out" 2>&1; then
		ok "headscale: HEADSCALE_CONFIG reads the mounted config and its policy"
	else
		fail "headscale: HEADSCALE_CONFIG reads the mounted config and its policy"; tail -3 "$tmp/out"
	fi
	if docker run --rm -v "$tmp:/config:ro" -e HEADSCALE_CONFIG=/config/headscale.yaml \
		-e HEADSCALE_POLICY_PATH=/config/absent.json --entrypoint headscale "$img" configtest >"$tmp/out" 2>&1; then
		fail "headscale: HEADSCALE_POLICY_PATH overrides the policy path"
	elif grep -q '/config/absent.json' "$tmp/out"; then
		ok "headscale: HEADSCALE_POLICY_PATH overrides the policy path"
	else
		fail "headscale: HEADSCALE_POLICY_PATH overrides the policy path"; tail -3 "$tmp/out"
	fi
	if docker run --rm --entrypoint headscale "$img" configtest >"$tmp/out" 2>&1; then
		ok "headscale: without overrides the baked placeholder config is used"
	else
		fail "headscale: without overrides the baked placeholder config is used"; tail -3 "$tmp/out"
	fi
}

exit_node_tests() {
	local img=headlamp-test/exit-node
	docker build -q --build-arg TSVERSION="$(cat "$root/TAILSCALE_VERSION")" -t "$img" "$root/apps/fly/exit-ams" >/dev/null ||
		{ fail "exit-node: image builds"; return; }
	printf 'mounted.example.com\n' >"$tmp/direct-hosts.txt"
	chmod 644 "$tmp/direct-hosts.txt"
	# Stub dig and tailscale: the test checks which file is read and what would be advertised, nothing else.
	local stub='mkdir -p /tmp/bin
printf "#!/bin/sh\necho 192.0.2.10\n" >/tmp/bin/dig
printf "#!/bin/sh\necho \"\$@\" >/tmp/advertised\n" >/app/tailscale
chmod +x /tmp/bin/dig /app/tailscale
PATH=/tmp/bin:$PATH /app/update-direct-routes.sh >/tmp/log 2>&1
grep -h "Resolving\|No routes resolved\|not found" /tmp/log; cat /tmp/advertised 2>/dev/null; true'
	docker run --rm -v "$tmp:/config:ro" -e DIRECT_HOSTS_FILE=/config/direct-hosts.txt --entrypoint sh "$img" -c "$stub" \
		>"$tmp/out" 2>&1
	if grep -q 'Resolving mounted.example.com' "$tmp/out" && grep -q -- '--advertise-routes=192.0.2.10/32' "$tmp/out"; then
		ok "exit-node: DIRECT_HOSTS_FILE reads the mounted list"
	else
		fail "exit-node: DIRECT_HOSTS_FILE reads the mounted list"; cat "$tmp/out"
	fi
	docker run --rm --entrypoint sh "$img" -c "$stub" >"$tmp/out" 2>&1
	# The baked placeholder holds only comments: the script finds the file, resolves nothing and advertises nothing.
	if grep -q 'No routes resolved' "$tmp/out" && ! grep -q 'mounted.example.com\|not found' "$tmp/out"; then
		ok "exit-node: without DIRECT_HOSTS_FILE the baked list is used"
	else
		fail "exit-node: without DIRECT_HOSTS_FILE the baked list is used"; cat "$tmp/out"
	fi
}

case "$which" in
headscale) headscale_tests ;;
exit-node) exit_node_tests ;;
all) headscale_tests; exit_node_tests ;;
*) echo "usage: $0 [headscale|exit-node|all]" >&2; exit 2 ;;
esac
[ "$fails" -eq 0 ] && echo "all runtime-config tests passed" || { echo "$fails failed"; exit 1; }

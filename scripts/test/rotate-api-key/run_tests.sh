#!/usr/bin/env bash
# Tests for the route approver's error reporting and the rotation script's argument handling (no network: curl is stubbed).
set -u
here="$(cd "$(dirname "$0")" && pwd)"; root="$here/../../.."
approve="$root/apps/fly/headscale/approve-gateway-routes.sh"; rotate="$root/scripts/rotate-headscale-api-key.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
ok()   { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

stub() { # http-code body : a curl that honours -o and -w '%{http_code}'
  mkdir -p "$tmp/bin"
  cat > "$tmp/bin/curl" <<STUB
#!/usr/bin/env bash
while [ \$# -gt 0 ]; do case "\$1" in -o) o="\$2"; shift;; esac; shift; done
printf '%s' '$2' > "\$o"; printf '%s' '$1'
STUB
  chmod +x "$tmp/bin/curl"
}
export HEADSCALE_URL=https://hs.example HEADSCALE_API_KEY=k

stub 401 '{}'
out="$(PATH="$tmp/bin:$PATH" "$approve" 2>&1)"; rc=$?
[ "$rc" = 2 ] && ok "approver exits 2 on 401" || fail "approver exit $rc on 401"
echo "$out" | grep -q "rejected the API key (HTTP 401)" && ok "401 names the expired key" || fail "401 message: $out"

stub 500 '{}'
out="$(PATH="$tmp/bin:$PATH" "$approve" 2>&1)"; rc=$?
[ "$rc" = 2 ] && echo "$out" | grep -q "HTTP 500" && ok "approver reports other HTTP errors" || fail "500 handling: $rc $out"

stub 200 '{"nodes":[]}'
out="$(PATH="$tmp/bin:$PATH" "$approve" 2>&1)"; rc=$?
[ "$rc" = 0 ] && echo "$out" | grep -q "No nodes found" && ok "approver succeeds with no matching nodes" || fail "200 handling: $rc $out"

stub 200 '{"nodes":[{"id":"1","name":"n","tags":["tag:gateway"],"availableRoutes":["10.0.0.0/24"]}]}'
out="$(PATH="$tmp/bin:$PATH" "$approve" 2>&1)"; rc=$?
[ "$rc" = 0 ] && echo "$out" | grep -q "Approved routes for node 1" && ok "approver approves routes (POST path)" || fail "approve path: $rc $out"

HS_APP= "$rotate" --status >/dev/null 2>&1; [ $? -ne 0 ] && ok "rotate requires HS_APP" || fail "HS_APP not enforced"
HS_APP=x "$rotate" --bogus >/dev/null 2>&1; [ $? = 64 ] && ok "rotate rejects unknown modes" || fail "bad mode accepted"
out="$(HS_APP=x STAGE_APPS="a b" HS_URL=https://hs.example DEPLOY_CMD="deploy a" "$rotate" --dry-run)"
echo "$out" | grep -q "on: a b" && echo "$out" | grep -q "deploy a" && ok "dry-run shows the configured steps" || fail "dry-run output: $out"
HS_APP=x "$rotate" >/dev/null 2>&1; [ $? -ne 0 ] && ok "rotate requires HS_URL and STAGE_APPS" || fail "rotate without config"
exit "$fails"

#!/usr/bin/env bash
# Tests for the rotation script's argument handling (no network).
set -u
here="$(cd "$(dirname "$0")" && pwd)"; root="$here/../../.."
rotate="$root/scripts/rotate-headscale-api-key.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
ok()   { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

HS_APP='' "$rotate" --status >/dev/null 2>&1; [ $? -ne 0 ] && ok "rotate requires HS_APP" || fail "HS_APP not enforced"
HS_APP=x "$rotate" --bogus >/dev/null 2>&1; [ $? = 64 ] && ok "rotate rejects unknown modes" || fail "bad mode accepted"
out="$(HS_APP=x STAGE_APPS="a b" HS_URL=https://hs.example DEPLOY_CMD="deploy a" "$rotate" --dry-run)"
echo "$out" | grep -q "on: a b" && echo "$out" | grep -q "deploy a" && ok "dry-run shows the configured steps" || fail "dry-run output: $out"
HS_APP=x "$rotate" >/dev/null 2>&1; [ $? -ne 0 ] && ok "rotate requires HS_URL and STAGE_APPS" || fail "rotate without config"
exit "$fails"

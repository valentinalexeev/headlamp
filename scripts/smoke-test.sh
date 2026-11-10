#!/bin/sh
# Smoke test of a site after an upgrade (#196). Run it from a tailnet node whose login may operate devices on
# node-control (the same as for scripts/headscale-policy.sh). It reports each step and exits 1 when one fails.
#
#   NODE_CONTROL_URL=http://node-control scripts/smoke-test.sh [--link-host <name>] [--keep]
#
# Steps:
#   1. bridges     every approved bridge is in sync, and the self-test of its agent (#196) can reach the internet
#                  through its exit node. An agent before 0.15.2 has no self-test: the step says "no self-test".
#   2. device      a test device is created, its subscription answers through link_host, and it is revoked. The
#                  revoke is pushed to every bridge (sync not pending). --keep leaves the device for a manual check.
#   3. public      the public enrolment paths of link_host reach node-control (#260): /me answers, an unknown path
#                  gets 404 from the edge. --link-host defaults to the host of the subscription URL of step 2.
#
# Env: NODE_CONTROL_URL (required), CURL (default curl), SMOKE_DEVICE_HOURS (default 1, the test device's lifetime).
set -u
base=${NODE_CONTROL_URL:?set NODE_CONTROL_URL to the base URL of node-control on the tailnet}
curl=${CURL:-curl}
hours=${SMOKE_DEVICE_HOURS:-1}
link=""
keep=false
while [ $# -gt 0 ]; do
  case "$1" in
    --link-host) link=$2; shift ;;
    --keep) keep=true ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
command -v jq >/dev/null 2>&1 || { echo "jq is needed" >&2; exit 2; }

pass=0; fails=0
ok()  { echo "ok   $1"; pass=$((pass + 1)); }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
api() { # METHOD PATH [JSON]: writes the body to $T/body, prints the status. A write always sends JSON (the console
  # refuses another Content-Type, as a CSRF guard).
  if [ "$1" = GET ]; then
    $curl -sS -o "$T/body" -w '%{http_code}' --max-time 30 "$base$2"
  else
    $curl -sS -o "$T/body" -w '%{http_code}' --max-time 30 -X "$1" -H 'Content-Type: application/json' --data "${3:-{\}}" "$base$2"
  fi
}

echo "# 1. bridges"
code=$(api GET /enroll/ingresses)
[ "$code" = 200 ] || { nok "GET /enroll/ingresses answered $code: $(head -c 200 "$T/body")"; echo "$pass passed, $fails failed"; exit 1; }
cp "$T/body" "$T/ingresses.json"
for name in $(jq -r '.ingresses[] | select(.approval == "approved" or .approval == "configured") | .name' "$T/ingresses.json"); do
  s=$(jq -r --arg n "$name" '.ingresses[] | select(.name == $n) | if .sync.pending then .sync.state else "ok" end' "$T/ingresses.json")
  [ "$s" = ok ] && ok "$name is in sync" || nok "$name is out of sync ($s)"
  e=$(jq -c --arg n "$name" '.egress[$n] // empty' "$T/ingresses.json")
  if [ -z "$e" ]; then
    echo "note $name: no self-test (an agent before 0.15.2, or ingress_egress_socks is empty)"
  elif [ "$(printf '%s' "$e" | jq -r .ok)" = true ]; then
    ok "$name reaches the internet through its exit node (checked $(printf '%s' "$e" | jq -r '.checked | todate'))"
  else
    nok "$name cannot reach the internet through its exit node: $(printf '%s' "$e" | jq -r .error)"
  fi
done

echo "# 2. a test device"
exp=$(( $(date +%s) + hours * 3600 ))
name="smoke-$(date +%Y%m%d%H%M%S)"
code=$(api POST /enroll/devices "{\"name\":\"$name\",\"owner\":\"smoke-test\",\"client\":\"sing-box\",\"expires_at\":$exp}")
if [ "$code" != 200 ]; then
  nok "create $name answered $code: $(head -c 200 "$T/body")"
else
  id=$(jq -r .id "$T/body"); sub=$(jq -r .subscription_url "$T/body")
  pending=$(jq -r '.sync_pending' "$T/body")
  [ "$pending" = false ] && ok "created $name (id $id), pushed to every bridge" || nok "created $name, but the push is pending: $(jq -c '[.ingresses[] | select(.sync.pending) | .name]' "$T/body")"
  [ -n "$link" ] || link=$(printf '%s' "$sub" | sed -n 's#^https://\([^/]*\)/.*#\1#p')
  c=$($curl -s -o "$T/sub" -w '%{http_code}' --max-time 20 -A smoke-test "$sub")
  if [ "$c" = 200 ] && [ -s "$T/sub" ]; then ok "the subscription answers through $link ($(wc -c < "$T/sub") bytes)"
  else nok "the subscription through $link answered $c"; fi
  if $keep; then
    echo "note $name is kept (--keep): revoke it in the console when you are done"
  else
    code=$(api POST "/enroll/devices/$id/revoke")
    if [ "$code" = 200 ] && [ "$(jq -r '.sync_pending' "$T/body")" = false ]; then ok "revoked $name on every bridge"
    else nok "revoke $name answered $code, sync pending $(jq -r '.sync_pending' "$T/body" 2>/dev/null)"; fi
    c=$($curl -s -o /dev/null -w '%{http_code}' --max-time 20 -A smoke-test "$sub")
    [ "$c" = 404 ] && ok "the revoked subscription answers 404" || nok "the revoked subscription answered $c"
  fi
fi

echo "# 3. public paths of ${link:-<no link host>}"
if [ -z "$link" ]; then
  nok "no link host: pass --link-host"
else
  c=$($curl -s -o /dev/null -w '%{http_code}' --max-time 20 "https://$link/me")
  case "$c" in
    200) ok "https://$link/me reaches node-control" ;;
    404) echo "note https://$link/me answers 404: self-enrolment is off, or the edge lacks the path (#260)" ;;
    *) nok "https://$link/me answered $c" ;;
  esac
  c=$($curl -s -o /dev/null -w '%{http_code}' --max-time 20 "https://$link/smoke-test-not-a-path")
  [ "$c" = 404 ] && ok "an unknown path gets 404 from the edge" || nok "an unknown path answered $c"
fi

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

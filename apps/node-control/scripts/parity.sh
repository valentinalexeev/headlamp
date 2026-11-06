#!/bin/sh
# Compare the read-only JSON endpoints of the Python node-control and the Go one.
#
# usage: parity.sh <python-url> <go-url>
#
# Fetches /nodes.json, /status, /tailnet/status and /enroll/devices (GET only) from both
# URLs, normalises each body with `jq -S` (sorted keys, so key order never differs) and prints a
# unified diff per path. A path that answers 404 on both sides is skipped (enrolment disabled).
# Exit 0: no differences. Exit 1: differences or a fetch failure. Exit 2: usage or missing tool.
#
# WARNING: the /enroll/devices diff can contain subscription data (device names, owners, links).
# Do not paste or share it.
#
# Requires: curl, jq. Nothing is modified on either side.
set -u

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <python-url> <go-url>" >&2
  exit 2
fi
for tool in curl jq diff; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$0: $tool is required" >&2; exit 2; }
done

case $1 in -*) echo "$0: arguments must not start with -" >&2; exit 2 ;; esac
case $2 in -*) echo "$0: arguments must not start with -" >&2; exit 2 ;; esac
py=${1%/}
go=${2%/}
tmp=$(mktemp -d) || exit 2
trap 'rm -rf "$tmp"' EXIT

# fetch <base-url> <path> <out-file>: prints the HTTP status; the normalised body goes to <out-file>.
fetch() {
  code=$(curl -sS --max-time 30 -o "$3.raw" -w '%{http_code}' "$1$2") || { echo "000"; return; }
  if [ "$code" = "200" ] && jq -S . "$3.raw" >"$3" 2>/dev/null; then
    :
  else
    : >"$3"
  fi
  echo "$code"
}

status=0
for path in /nodes.json /status /tailnet/status /enroll/devices; do
  name=$(echo "$path" | tr '/' '_')
  pc=$(fetch "$py" "$path" "$tmp/py$name")
  gc=$(fetch "$go" "$path" "$tmp/go$name")
  if [ "$pc" = "404" ] && [ "$gc" = "404" ]; then
    echo "== $path: 404 on both, skipped"
    continue
  fi
  if [ "$pc" != "$gc" ]; then
    echo "== $path: HTTP status differs (python $pc, go $gc)"
    status=1
    continue
  fi
  if [ "$pc" != "200" ]; then
    echo "== $path: HTTP $pc on both, not comparable"
    status=1
    continue
  fi
  if [ ! -s "$tmp/py$name" ] || [ ! -s "$tmp/go$name" ]; then
    echo "== $path: a response is not valid JSON"
    status=1
    continue
  fi
  if diff -u --label "python$path" --label "go$path" "$tmp/py$name" "$tmp/go$name"; then
    echo "== $path: identical"
  else
    status=1
  fi
done
exit "$status"

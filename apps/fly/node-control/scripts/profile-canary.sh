#!/bin/sh
# Compare a canary device's subscription profile fetched two ways:
#   direct: <tailnet-base-url>:8081/sub/<secret>   (node-control's public listener, over the tailnet)
#   link:   https://<link-host>/sub/<secret>        (the path real phones use, through an ingress front)
# A difference means something on the link path serves phones different bytes than node-control
# produced. Run it from cron or CI on a machine that is on the tailnet.
#
# Usage: profile-canary.sh <tailnet-base-url> <link-host> [<canary-sub-secret> | -]
#   <tailnet-base-url>  http(s)://host, no port, path or credentials (the port 8081 is appended)
#   <link-host>         DNS name or IPv4 address, no scheme, port or path
#   <canary-sub-secret> the subscription secret of a dedicated canary device (<digits>.<token>).
#                       Prefer the environment variable CANARY_SECRET (no third argument) or `-` (read
#                       one line from stdin): an argument is visible to every local user in `ps`.
#                       Do not run this script with `sh -x`: the trace prints the secret.
#
# Each side must answer exactly 200 with a non-empty body; a redirect, an error status or an empty
# body is a fetch error (exit 2), never a match.
#
# Prints only two short SHA-256 prefixes and OK or MISMATCH; never a body and never the secret.
# Exit: 0 identical, 1 different, 2 usage or fetch error.
#
# Test-only hooks, honoured ONLY when CANARY_TEST=1 (otherwise ignored): CANARY_DIRECT_PORT
# (default 8081), CANARY_LINK_PORT (default none), CANARY_LINK_SCHEME (https or http, default https).
set -u

usage() { echo "usage: profile-canary.sh <tailnet-base-url> <link-host> <canary-sub-secret>" >&2; exit 2; }
die() { echo "profile-canary: $*" >&2; exit 2; }

[ $# -eq 2 ] || [ $# -eq 3 ] || usage
base=$1; host=$2
if [ $# -eq 3 ]; then
  secret=$3
else
  secret=${CANARY_SECRET:-}
  [ -n "$secret" ] || usage
fi
if [ "$secret" = "-" ]; then
  IFS= read -r secret || die "no secret on stdin"
fi

# control characters (a newline would hide a second line from the line-based checks below) and
# a leading '-' are refused outright
for v in "$base" "$host" "$secret"; do
  case $v in
    *[[:cntrl:]]*) die "arguments must not contain control characters" ;;
    -*) die "arguments must not start with '-'" ;;
  esac
done

printf '%s\n' "$base" | grep -Eq '^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$' ||
  die "base url must look like http://name (no port, path or credentials)"
printf '%s\n' "$host" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$' ||
  die "link host must be a DNS name (no scheme, port or path)"
printf '%s\n' "$secret" | grep -Eq '^[0-9]+\.[A-Za-z0-9_-]+$' ||
  die "secret has the wrong shape (expected <digits>.<token>)"

dport=8081
lport=
scheme=https
if [ "${CANARY_TEST:-}" = 1 ]; then # test-only hooks
  dport=${CANARY_DIRECT_PORT:-8081}
  lport=${CANARY_LINK_PORT:-}
  scheme=${CANARY_LINK_SCHEME:-https}
fi
case $dport in ''|*[!0-9]*) die "bad CANARY_DIRECT_PORT" ;; esac
case $lport in *[!0-9]*) die "bad CANARY_LINK_PORT" ;; esac
case $scheme in http|https) ;; *) die "bad CANARY_LINK_SCHEME" ;; esac

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  hash256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  hash256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required"
fi

tmp=$(mktemp -d) || die "cannot create a temp dir"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 2' INT TERM
umask 077

direct_url="$base:$dport/sub/$secret"
link_url="$scheme://$host${lport:+:$lport}/sub/$secret"

# fetch <name> <url>: the URL goes to curl through a config on stdin, so the secret is never in
# the process list; curl's own messages are discarded (they can repeat the URL). Redirects are not
# followed, and only a 200 with a non-empty body counts as a profile.
fetch() {
  code=$(printf 'url = "%s"\n' "$2" |
    curl --config - --max-time 20 --silent --show-error \
      --header "User-Agent: profile-canary" --output "$tmp/$1" --write-out '%{http_code}' 2>/dev/null)
  rc=$?
  [ $rc -eq 0 ] || die "fetching the $1 profile failed (curl exit $rc)"
  [ "$code" = 200 ] || die "the $1 side answered HTTP $code, not 200"
  [ -s "$tmp/$1" ] || die "the $1 side answered with an empty body"
}

fetch direct "$direct_url"
fetch link "$link_url"

h1=$(hash256 "$tmp/direct") || die "hashing failed"
h2=$(hash256 "$tmp/link") || die "hashing failed"
s1=$(printf '%s' "$h1" | cut -c1-12)
s2=$(printf '%s' "$h2" | cut -c1-12)

if [ "$h1" = "$h2" ]; then
  echo "OK direct=$s1 link=$s2"
  exit 0
fi
echo "MISMATCH direct=$s1 link=$s2"
exit 1

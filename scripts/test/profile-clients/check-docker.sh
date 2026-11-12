#!/bin/sh
# On-demand check of the profile goldens with the real clients (#313). Run it with
# `make test-profile-clients`. Needs docker.
#
# The goldens hold fake Reality keys (KEY-...-fake), which the clients refuse. The test puts one
# generated public key in their place, in a copy, and runs:
#   - `sing-box check` on each sing-box golden;
#   - `mihomo -t` on each Clash golden.
#   E2E_SINGBOX_IMAGE  default ghcr.io/sagernet/sing-box:v1.14.4
#   E2E_MIHOMO_IMAGE   default metacubex/mihomo:v1.19.33
set -eu

here=$(cd "$(dirname "$0")" && pwd)
td=$(cd "$here/../../../apps/node-control/testdata" && pwd)
sb=${E2E_SINGBOX_IMAGE:-ghcr.io/sagernet/sing-box:v1.14.4}
mh=${E2E_MIHOMO_IMAGE:-metacubex/mihomo:v1.19.33}
pass=0
fail=0

if ! docker info >/dev/null 2>&1; then
  echo "# skip: docker is not available"
  exit 0
fi

T=$(mktemp -d "${TMPDIR:-/tmp}/profile-clients.XXXXXX")
trap 'rm -rf "$T"' EXIT INT TERM

key=$(docker run --rm "$sb" generate reality-keypair | awk '/PublicKey/ {print $2}')
[ -n "$key" ] || { echo "not ok - no Reality key from $sb"; exit 1; }

for f in "$td"/profile_*singbox*.golden "$td"/profile_*clash*.golden; do
  name=$(basename "$f")
  # The escape fixtures hold odd characters on purpose, to test the JSON escaping. They are not
  # meant to be valid client configs.
  case $name in *_escape.golden) continue ;; esac
  sed "s/KEY-[A-Z]*-fake/$key/g" "$f" > "$T/$name"
  case $name in
    *singbox*) cmd="docker run --rm -v $T:/t:ro $sb check -c /t/$name" ;;
    *) cmd="docker run --rm -v $T:/t:ro $mh -t -f /t/$name" ;;
  esac
  if out=$($cmd 2>&1) && ! printf '%s' "$out" | grep -qi "test failed"; then
    pass=$((pass + 1)); echo "ok - $name"
  else
    fail=$((fail + 1)); echo "not ok - $name"; printf '%s\n' "$out" | tail -3 | sed 's/^/#   /'
  fi
done

echo "# $pass passed, $fail failed"
[ "$fail" -eq 0 ]

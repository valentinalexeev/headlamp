#!/usr/bin/env bash
# Tests for scripts/check-denylist.sh with a throwaway pattern list (harmless words only).
set -u
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../../check-denylist.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fails=0
ok()   { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }
expect() { # name expected-exit command...
  local name="$1" want="$2"; shift 2
  "$@" >"$tmp/out" 2>&1; local got=$?
  [ "$got" = "$want" ] && ok "$name" || { fail "$name (exit $got, wanted $want)"; sed 's/^/       /' "$tmp/out"; }
}

printf '# comment\n\nzzforbidden\nbar+baz\n' > "$tmp/list"
export HEADLAMP_DENYLIST="$tmp/list"

printf 'a clean sentence about relays\n' > "$tmp/clean.txt"
printf 'mentions ZZFORBIDDEN in caps\n' > "$tmp/bad1.txt"
printf 'mentions barrrbaz here\n' > "$tmp/bad2.txt"
expect "clean text passes"            0 "$script" --text "$tmp/clean.txt"
expect "case-insensitive hit"         1 "$script" --text "$tmp/bad1.txt"
expect "regex hit"                    1 "$script" --text "$tmp/bad2.txt"
expect "stdin text"                   1 bash -c "printf 'zzforbidden\n' | '$script' --text -"
expect "commit message hit"           1 "$script" --message "$tmp/bad1.txt"
expect "commit message clean"         0 "$script" --message "$tmp/clean.txt"
expect "bad usage"                   64 "$script" --nonsense

git init -q "$tmp/repo" && git -C "$tmp/repo" config user.email t@example.com && git -C "$tmp/repo" config user.name t
pushd "$tmp/repo" >/dev/null || exit 1
echo "fine" > ok.txt && git add ok.txt
expect "staged clean"               0 "$script" --staged
echo "zzforbidden" > bad.txt && git add bad.txt
expect "staged hit in content"      1 "$script" --staged
git rm -q --cached bad.txt && rm bad.txt
echo "fine" > zzforbidden-name.txt && git add zzforbidden-name.txt
expect "staged hit in a path"       1 "$script" --staged
popd >/dev/null || exit 1

HEADLAMP_DENYLIST="$tmp/missing" expect "missing list warns, passes"        0 "$script" --text "$tmp/bad1.txt"
HEADLAMP_DENYLIST="$tmp/missing" HEADLAMP_DENYLIST_REQUIRED=1 expect "missing list, required" 2 "$script" --text "$tmp/bad1.txt"

[ "$fails" = 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }

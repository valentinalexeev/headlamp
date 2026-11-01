#!/usr/bin/env bash
# Tests for the release scripts, in a throwaway git repository.
set -uo pipefail

root=$(cd "$(dirname "$0")/../../.." && pwd)
check="$root/scripts/check-release.sh"
tags="$root/scripts/ci-image-tags.sh"
conv="$root/scripts/check-conventional.sh"
comp="$root/scripts/ci-components.sh"
failed=0

ok() { if "$@" >/dev/null 2>&1; then echo "ok   $*"; else echo "FAIL expected success: $*"; failed=1; fi; }
no() { if "$@" >/dev/null 2>&1; then echo "FAIL expected failure: $*"; failed=1; else echo "ok   refused: $*"; fi; }
same() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got [$2], want [$3]"; failed=1; fi; }

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cd "$dir" || exit 1
git init -q -b main .
git config user.name t
git config user.email t@example.net
commit() { # MESSAGE [VERSION]
	[ -z "${2:-}" ] || echo "$2" >VERSION
	echo "$1" >>log
	git add -A
	git commit -qm "$1"
}
# shellcheck disable=SC2329 # called through ok and no
try() { # TAG: tags HEAD, runs the check, removes the tag again
	git tag "$1"
	"$check" "$1" main
	local rc=$?
	git tag -d "$1" >/dev/null
	return "$rc"
}

# conventional titles
ok "$conv" 'feat(api): add a thing'
ok "$conv" 'fix: a thing'
ok "$conv" 'refactor(core)!: drop a thing'
no "$conv" 'Add a thing'
no "$conv" 'doc(CD): a thing'
no "$conv" 'feat:no space'
no "$conv" ''

# the first release
commit 'chore: start' 0.9.0
no try 0.9.0
no try v0.9
no try v0.09.0
no try v0.9.0+build
no try v0.9.0-beta
no try v1.0.0
ok try v0.9.0
ok try v0.9.0-rc.1
git tag v0.9.0

# a fix needs only a patch, and versions cannot be skipped
commit 'fix: a thing' 0.9.1
ok try v0.9.1
commit 'chore: skip' 0.9.2
no try v0.9.2
echo 0.9.1 >VERSION
git add -A
git commit -qm 'chore: back'
ok try v0.9.1
git tag v0.9.1

# a feature needs a minor
commit 'feat(api): a thing' 0.9.2
no try v0.9.2
commit 'chore: minor' 0.10.0
ok try v0.10.0
git tag v0.10.0

# before 1.0.0 a breaking change needs a minor; after it, a major
commit 'refactor(core)!: drop a thing' 0.10.1
no try v0.10.1
commit 'chore: minor' 0.11.0
ok try v0.11.0
git tag v0.11.0
commit 'chore: release' 1.0.0
ok try v1.0.0
git tag v1.0.0
commit 'refactor(core)!: drop a thing' 1.1.0
no try v1.1.0
commit 'chore: major' 2.0.0
ok try v2.0.0
git tag v2.0.0
printf 'fix: a thing\n\nBREAKING CHANGE: a footer counts too\n' >msg
echo x >>log
echo 2.0.1 >VERSION
git add log VERSION
git commit -qF msg
no try v2.0.1
rm -f msg
git reset -q --hard HEAD~1

# the tag must be on main
git checkout -qb side
commit 'fix: side' 2.0.1
git checkout -q main
git tag v9.9.9 side
no "$check" v9.9.9 main
git tag -d v9.9.9 >/dev/null

# a release cannot go out while its milestone has open issues
git checkout -q side
git branch -qf main side
git checkout -q main
mkdir bin
# shellcheck disable=SC2016 # the stub expands it when it runs
printf '#!/bin/sh\necho "${OPEN_ISSUES:-}"\n' >bin/gh
chmod +x bin/gh
git tag v2.0.1
OPEN_ISSUES=3 RELEASE_REPO=o/r PATH="$PWD/bin:$PATH" no "$check" v2.0.1 main
OPEN_ISSUES=0 RELEASE_REPO=o/r PATH="$PWD/bin:$PATH" ok "$check" v2.0.1 main
OPEN_ISSUES='' RELEASE_REPO=o/r PATH="$PWD/bin:$PATH" ok "$check" v2.0.1 main
rm -rf bin

# image tags
same 'branch tags' "$("$tags" branch 0123456789abcdef)" $'main\nsha-0123456789ab'
same 'prerelease' "$("$tags" tag v3.0.0-rc.1)" '3.0.0-rc.1'
same 'before 1.0.0 nothing but the minor floats' "$(git tag v0.9.9; "$tags" tag v0.12.0)" $'0.12.0\n0.12'
same 'the newest stable floats' "$("$tags" tag v3.0.0)" $'3.0.0\n3.0\n3\nlatest'
same 'an older line does not move latest' "$("$tags" tag v1.0.1)" $'1.0.1\n1.0\n1'
same 'an older patch moves nothing' "$(git tag v1.0.2; "$tags" tag v1.0.1)" '1.0.1'

# components: one source changing rebuilds one image, and a shared path rebuilds both
mkdir -p .github a b scripts
cat >.github/components.json <<'JSON'
[{"image":"one","context":"a","dockerfile":"Dockerfile","paths":["a"]},
 {"image":"two","context":"b","dockerfile":"Dockerfile","paths":["b","shared"],"args":{"K":"V"},"args_from_files":{"W":"shared"},"go":true,"test":"true"}]
JSON
echo 1 >a/f
echo 1 >b/f
echo 7 >shared
git add -A
git commit -qm 'chore: components'
base=$(git rev-parse HEAD)
listed=$("$comp" list)
same 'list has both' "$(jq -c '[.[].image]' <<<"$listed")" '["one","two"]'
same 'build args combine' "$(jq -r '.[1].build_args' <<<"$listed")" $'K=V\nW=7'
same 'nothing changed' "$("$comp" changed "$base")" '[]'
echo 2 >a/f
git add -A
git commit -qm 'fix: a'
same 'a changed' "$("$comp" changed "$base" | jq -c '[.[].image]')" '["one"]'
same 'a keeps b hash' "$("$comp" list | jq -r '.[1].hash')" "$(jq -r '.[1].hash' <<<"$listed")"
same 'a moves a hash' "$([ "$("$comp" list | jq -r '.[0].hash')" != "$(jq -r '.[0].hash' <<<"$listed")" ] && echo moved)" moved
echo 8 >shared
git add -A
git commit -qm 'fix: shared'
same 'a path outside the context counts' "$("$comp" changed "$base" | jq -c '[.[].image]')" '["one","two"]'
mkdir -p .github/workflows
echo x >.github/workflows/images.yml
git add -A
git commit -qm 'ci: build'
base=$(git rev-parse HEAD~1)
same 'changing the build means all' "$("$comp" changed "$base" | jq -c '[.[].image]')" '["one","two"]'

exit $failed

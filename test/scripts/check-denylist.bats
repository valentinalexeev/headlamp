#!/usr/bin/env bats
# Tests for scripts/check-denylist.sh with a throwaway pattern list (harmless words only). Ported from
# scripts/test/check-denylist/run_tests.sh (ADR 0004).

setup() {
  load ../helpers
  script="$REPO/scripts/check-denylist.sh"
  T="$BATS_TEST_TMPDIR"
  printf '# comment\n\nzzforbidden\nbar+baz\n' > "$T/list"
  export HEADLAMP_DENYLIST="$T/list"
  printf 'a clean sentence about relays\n' > "$T/clean.txt"
  printf 'mentions ZZFORBIDDEN in caps\n' > "$T/bad1.txt"
  printf 'mentions barrrbaz here\n' > "$T/bad2.txt"
}

@test "clean text passes" {
  run "$script" --text "$T/clean.txt"
  assert_success
}

@test "a hit is case-insensitive" {
  run "$script" --text "$T/bad1.txt"
  assert_failure 1
}

@test "patterns are regular expressions" {
  run "$script" --text "$T/bad2.txt"
  assert_failure 1
}

@test "text on stdin" {
  run bash -c "printf 'zzforbidden\n' | '$script' --text -"
  assert_failure 1
}

@test "commit message: hit and clean" {
  run "$script" --message "$T/bad1.txt"
  assert_failure 1
  run "$script" --message "$T/clean.txt"
  assert_success
}

@test "bad usage exits 64" {
  run "$script" --nonsense
  assert_failure 64
}

@test "staged changes: clean, a hit in content, a hit in a path" {
  git init -q "$T/repo"
  cd "$T/repo"
  git config user.email t@example.com
  git config user.name t
  echo fine > ok.txt && git add ok.txt
  run "$script" --staged
  assert_success

  echo zzforbidden > bad.txt && git add bad.txt
  run "$script" --staged
  assert_failure 1
  git rm -q --cached bad.txt && rm bad.txt

  echo fine > zzforbidden-name.txt && git add zzforbidden-name.txt
  run "$script" --staged
  assert_failure 1
}

@test "a missing list warns and passes, unless it is required" {
  HEADLAMP_DENYLIST="$T/missing" run "$script" --text "$T/bad1.txt"
  assert_success
  HEADLAMP_DENYLIST="$T/missing" HEADLAMP_DENYLIST_REQUIRED=1 run "$script" --text "$T/bad1.txt"
  assert_failure 2
}

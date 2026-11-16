#!/usr/bin/env bats
# Tests for scripts/check-set-eu.sh (#368, ADR 0002 rule 8), on a throwaway git repo.

setup() {
  load ../helpers
  script="$REPO/scripts/check-set-eu.sh"
  T="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$T/scripts"
  git -C "$T" init -q
  printf '#!/bin/sh\nset -eu\necho ok\n' > "$T/scripts/good.sh"
  printf '#!/usr/bin/env bash\nset -euo pipefail\necho ok\n' > "$T/scripts/goodbash.sh"
  printf '# harnesses not moved yet\n' > "$T/scripts/check-set-eu.allow"
  git -C "$T" add -A
}

@test "scripts with the flags pass" {
  run "$script" "$T"
  assert_success
}

@test "a POSIX script without set -e fails" {
  printf '#!/bin/sh\nset -u\n' > "$T/scripts/bad.sh"
  git -C "$T" add scripts/bad.sh
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "scripts/bad.sh has no 'set -eu'"
}

@test "a bash script without pipefail fails" {
  printf '#!/usr/bin/env bash\nset -eu\n' > "$T/scripts/nopf.sh"
  git -C "$T" add scripts/nopf.sh
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "scripts/nopf.sh has no 'set -euo pipefail'"
}

@test "set -o pipefail on its own line counts" {
  printf '#!/bin/bash\nset -eu\nset -o pipefail\n' > "$T/scripts/split.sh"
  git -C "$T" add scripts/split.sh
  run "$script" "$T"
  assert_success
}

@test "a listed harness without the flags passes" {
  printf '#!/bin/sh\necho old\n' > "$T/scripts/old.sh"
  printf 'scripts/old.sh\n' >> "$T/scripts/check-set-eu.allow"
  git -C "$T" add -A
  run "$script" "$T"
  assert_success
}

@test "a listed file that has the flags must leave the list" {
  printf 'scripts/good.sh\n' >> "$T/scripts/check-set-eu.allow"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "remove it from"
}

@test "a listed file that does not exist fails" {
  printf 'scripts/gone.sh\n' >> "$T/scripts/check-set-eu.allow"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "does not exist"
}

@test "the repository passes" {
  run "$script" "$REPO"
  assert_success
}

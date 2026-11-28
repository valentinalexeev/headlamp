#!/usr/bin/env bats
# Tests for scripts/check-vendored.sh (#429), on a throwaway git repo.

setup() {
  load ../helpers
  script="$REPO/scripts/check-vendored.sh"
  T="$BATS_TEST_TMPDIR/repo"
  V="$T/app/static/vendor"
  mkdir -p "$V"
  git -C "$T" init -q
  printf '/* lib */ lib.version="1.2.3";\n' > "$V/lib.min.js"
  printf 'MIT\n' > "$V/LICENSE"
  {
    echo "# header comment"
    echo "lib.min.js 1.2.3 $(sha256sum "$V/lib.min.js" | cut -d' ' -f1) https://registry.example.com/lib-1.2.3.tgz"
    echo "LICENSE - $(sha256sum "$V/LICENSE" | cut -d' ' -f1) https://registry.example.com/lib-1.2.3.tgz"
  } > "$V/VENDORED"
  git -C "$T" add -A
}

@test "the pinned files pass" {
  run "$script" "$T"
  assert_success
  assert_output --partial "ok       app/static/vendor/lib.min.js 1.2.3"
}

@test "a changed vendored file fails" {
  printf '/* lib */ lib.version="1.2.3"; evil();\n' > "$V/lib.min.js"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "CHANGED  app/static/vendor/lib.min.js"
}

@test "a changed licence fails" {
  printf 'not MIT\n' > "$V/LICENSE"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "CHANGED  app/static/vendor/LICENSE"
}

@test "a file that does not declare the pinned version fails" {
  printf '/* lib */ lib.version="1.2.4";\n' > "$V/lib.min.js"
  sed -i "s/^lib.min.js 1.2.3 [0-9a-f]*/lib.min.js 1.2.3 $(sha256sum "$V/lib.min.js" | cut -d' ' -f1)/" "$V/VENDORED"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial 'VERSION  app/static/vendor/lib.min.js does not declare version="1.2.3"'
}

@test "a missing listed file fails" {
  rm "$V/LICENSE"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "MISSING  app/static/vendor/LICENSE"
}

@test "a file that the manifest does not list fails" {
  printf 'x\n' > "$V/extra.js"
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "UNLISTED app/static/vendor/extra.js"
}

@test "a repo with no manifest fails" {
  git -C "$T" rm -q --cached app/static/vendor/VENDORED
  run "$script" "$T"
  assert_failure 1
  assert_output --partial "no VENDORED manifest is tracked"
}

@test "the vendored files of this repo pass" {
  run "$script" "$REPO"
  assert_success
  assert_output --partial "cytoscape.min.js 3.35.0"
}

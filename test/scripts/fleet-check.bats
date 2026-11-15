#!/usr/bin/env bats
# Tests for scripts/fleet-check.sh (#195) with a fake curl. No network. Ported from
# scripts/test/fleet-check/run_tests.sh (ADR 0004).

setup() {
  load ../helpers
  script="$REPO/scripts/fleet-check.sh"
  T="$BATS_TEST_TMPDIR"
  # The fake curl writes $FAKE/body to the -o file and prints $FAKE/code.
  cat > "$T/curl" <<'S'
#!/bin/sh
out=""
while [ $# -gt 0 ]; do [ "$1" = -o ] && { out=$2; shift; }; shift; done
cp "$FAKE/body" "$out"
cat "$FAKE/code"
S
  chmod +x "$T/curl"
  echo 200 > "$T/code"
  cat > "$T/body" <<'J'
{"node_control":"v0.16.0","headscale":"v0.29.7","tailscale_pin":"1.104.5",
 "nodes":[{"name":"relay-1","online":true,"tags":["tag:relay"],"tailscale":"1.104.5","status":"current"},
          {"name":"phone","online":false,"tailscale":"1.90.0","status":"older"}],
 "bridges":[{"name":"br-1","source":"discovered","online":true,"active":true,"sync":"ok","agent":"0.16.0+abc1234","xray":"25.10.15"}],
 "funnel":{"version":"0.16.0","reported_at":"2026-11-10T10:00:00Z","stale":false,"errors":0},
 "problems":[],"notes":[]}
J
}

fleet() { FAKE="$T" CURL="$T/curl" NODE_CONTROL_URL=http://nc sh "$script" "$@"; }

edit_body() { jq "$1" "$T/body" > "$T/b2" && mv "$T/b2" "$T/body"; }

@test "a healthy fleet exits 0, and devices are left out by default" {
  run fleet
  assert_success
  assert_output --partial "node-control"
  assert_output --partial "v0.29.7"
  assert_output --partial "agent 0.16.0+abc1234 xray 25.10.15"
  assert_output --partial "relay-1"
  assert_output --partial "no problems"
  refute_output --partial "phone"
}

@test "--all lists devices" {
  run fleet --all
  assert_success
  assert_output --partial "phone"
  assert_output --partial "older, offline"
}

@test "a problem exits 1" {
  edit_body '.problems = ["bridge br-1: offline"] | .notes = ["bridge x: the agent reports no version (before 0.16.0)"] | .bridges[0].online = false'
  run fleet
  assert_failure 1
  assert_output --partial "PROBLEM bridge br-1: offline"
  assert_output --partial "note: bridge x"
}

@test "control characters are removed, and --json prints the report" {
  edit_body '.problems = ["\u001b[31mred"]'
  run fleet
  refute_output --partial "$(printf '\033')"
  run fleet --json
  assert_failure 1
  assert_output --partial '"node_control"'
}

@test "compatibility shows on the rows, with runners and strict mode (#300)" {
  cat > "$T/body" <<'J'
{"node_control":"v0.18.0","tailscale_pin":"1.104.5","compat_strict":true,"nodes":[],
 "bridges":[{"name":"br-2","source":"discovered","online":true,"active":true,"sync":"ok","agent":"0.19.0","compat":{"interface":"agent-api","state":"incompatible","reported":2}}],
 "funnel":{"version":"0.16.0","reported_at":"2026-11-10T10:00:00Z","stale":false,"errors":0,"compat":{"interface":"funnel-api","state":"unknown"}},
 "runners":[{"address":"100.64.0.8","node":"runner-1","compat":{"interface":"runner-api","state":"incompatible","reported":5,"hint":"newer than node-control accepts: upgrade node-control"}}],
 "problems":["bridge br-2: agent-api version 2 is newer"],"notes":[]}
J
  run fleet
  assert_failure 1
  for t in "INCOMPATIBLE agent-api 2" "api unknown" "runner-1" "runner-api 5" \
    "incompatible newer than node-control accepts" "COMPAT_STRICT on"; do
    assert_output --partial "$t"
  done
}

@test "no access, an old node-control and an unknown argument exit 2" {
  echo 403 > "$T/code"
  echo '{"detail":"no"}' > "$T/body"
  run fleet
  assert_failure 2
  assert_output --partial "HTTP 403"

  echo 200 > "$T/code"
  echo '<html>' > "$T/body"
  run fleet
  assert_failure 2
  assert_output --partial "before 0.16.0"

  run fleet --bad
  assert_failure 2
  assert_output --partial "unknown argument"
}

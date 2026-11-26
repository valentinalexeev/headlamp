#!/usr/bin/env bats
# Tests of the tailguard image and its exit-node tracker (#391). The --decide cases run against the script in the
# checkout, or inside the image when TAILGUARD_IMAGE is set. The image tests need TAILGUARD_IMAGE and docker; without
# it they are skipped. `make test-tailguard` builds the image and runs all of them (CI: .github/components.json).

setup() {
  load ../helpers
  pin=$(tr -d '[:space:]' < "$REPO/TAILSCALE_VERSION")
}

# decide ROWS: the tracker's decision for the rows "ip name online selected tagged", with EXIT_NODE_PREFIX=my-exit.
decide() {
  if [ -n "${TAILGUARD_IMAGE:-}" ]; then
    printf '%b' "$1" | docker run --rm -i -e EXIT_NODE_PREFIX=my-exit --entrypoint sh "$TAILGUARD_IMAGE" \
      /usr/local/bin/track-exit-node.sh --decide
  else
    printf '%b' "$1" | EXIT_NODE_PREFIX=my-exit sh "$REPO/apps/tailguard/track-exit-node.sh" --decide
  fi
}

in_image() {
  [ -n "${TAILGUARD_IMAGE:-}" ] || skip "TAILGUARD_IMAGE is not set (make test-tailguard builds it)"
  docker run --rm --entrypoint sh "$TAILGUARD_IMAGE" -c "$1"
}

@test "decide: the selected exit node is online and tagged, so it is kept" {
  run decide '100.64.0.1 my-exit 1 1 1\n100.64.0.2 my-exit-1 1 0 1\n'
  assert_success
  assert_output "keep 100.64.0.1"
}

@test "decide: the selected exit node is offline, so the online tagged match is pinned" {
  run decide '100.64.0.1 my-exit 0 1 1\n100.64.0.2 my-exit-1 1 0 1\n'
  assert_success
  assert_output "pin 100.64.0.2"
}

@test "decide: the selected exit node lost the tag, so a tagged match is pinned" {
  run decide '100.64.0.1 my-exit 1 1 0\n100.64.0.2 my-exit-1 1 0 1\n'
  assert_success
  assert_output "pin 100.64.0.2"
}

@test "decide: a node that copies the name without the tag is not pinned" {
  run decide '100.64.0.3 my-exit 1 0 0\n100.64.0.2 my-exit-1 1 0 1\n'
  assert_success
  assert_output "pin 100.64.0.2"
}

@test "decide: no online tagged node with the prefix, so the tracker waits" {
  run decide '100.64.0.1 my-exit 0 0 1\n100.64.0.4 other-exit 1 0 1\n100.64.0.5 my-exit-2 1 0 0\n'
  assert_success
  assert_output "wait no-online-exit-node-matching-my-exit*-with-tag:exit"
}

@test "decide: no exit nodes at all, so the tracker waits" {
  run decide ''
  assert_success
  assert_output "wait no-online-exit-node-matching-my-exit*-with-tag:exit"
}

@test "image: tailscale and tailscaled are the repo pin" {
  run in_image 'tailscale version | head -n1; tailscaled --version | head -n1'
  assert_success
  assert_output "$(printf '%s\n%s' "$pin" "$pin")"
}

@test "image: the scripts are in /usr/local/bin" {
  run in_image 'for s in route-health.sh track-exit-node.sh verify.sh; do test -r /usr/local/bin/$s || echo missing $s; done'
  assert_success
  assert_output ""
}

@test "image: tailscaled starts and answers on its socket" {
  run in_image 'tailscaled --tun=userspace-networking --state=mem: --socket=/tmp/t.sock >/tmp/log 2>&1 &
    i=0; while [ $i -lt 30 ]; do
      tailscale --socket=/tmp/t.sock status >/tmp/st 2>&1
      if grep -qiE "logged out|NeedsLogin" /tmp/st; then echo started; exit 0; fi
      i=$((i + 1)); sleep 1
    done; cat /tmp/st /tmp/log; exit 1'
  assert_success
  assert_output "started"
}

@test "image: the tracker starts and stops on a missing TG_LAN_ACCESS" {
  [ -n "${TAILGUARD_IMAGE:-}" ] || skip "TAILGUARD_IMAGE is not set (make test-tailguard builds it)"
  run docker run --rm -e EXIT_NODE_PREFIX=my-exit --entrypoint sh "$TAILGUARD_IMAGE" /usr/local/bin/track-exit-node.sh
  assert_failure 64
  assert_output --partial "TG_LAN_ACCESS is not set"
}

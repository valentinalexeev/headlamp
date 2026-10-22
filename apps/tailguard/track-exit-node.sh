#!/bin/sh
# Exit-node tracker for tailguard.
#
# Sidecar sharing tailguard's network namespace AND its tailscaled socket
# (./run is bind-mounted at /tmp in both containers, where tailscaled keeps
# /tmp/tailscaled.sock). It talks to tailguard's own tailscaled -- not a second
# tailnet node -- so it sees the same netmap and can change the exit node.
#
# Why this exists: an exit node that re-registers comes back with a new Tailscale
# IP and possibly a suffixed name (my-exit-1). A
# hard-pinned IP goes stale and egress dies. This follows the node instead.
#
# Every INTERVAL seconds:
#   * if the selected exit node is online          -> leave it alone
#   * else if an online exit node named $PREFIX*   -> `tailscale set --exit-node`
#   * else                                          -> log and wait
#
# The exit node is NOT passed to tailguard at boot (docker-compose.yml blanks
# TS_EXIT_NODE): env_file values are baked in when the container is created, so
# a stale pin there would make `tailscale up --exit-node=<dead ip>` fail and
# crashloop tailguard after a reboot. This tracker pins it right after boot.
set -u

PREFIX="${EXIT_NODE_PREFIX:?set EXIT_NODE_PREFIX (name prefix of the exit node to follow)}"
INTERVAL="${TRACK_INTERVAL:-20}"      # seconds between checks
SETTLE="${TRACK_SETTLE:-45}"          # pause after a re-pin before acting again
SOCK="${TS_SOCKET_PATH:-/tmp/tailscaled.sock}"
PROBE="${TRACK_PROBE:-https://connectivitycheck.gstatic.com/generate_204}"
BLOCK_DERP="${BLOCK_DERP_REGIONS:-}"  # DERP region codes to never pick as home, e.g. "xyz"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
TS() { tailscale --socket="$SOCK" "$@"; }

# Exit-node-capable peers as "ip label online selected" (online/selected 0|1).
# `exit-node list` only prints peers that offer exit node; label = first DNS label.
nodes() {
  TS exit-node list 2>/dev/null | awk '
    $1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {
      h = $2; sub(/\..*/, "", h)
      print $1, h, ($0 ~ /offline/ ? 0 : 1), ($0 ~ /selected/ ? 1 : 0)
    }'
}

# stdin: rows from nodes(). stdout: "keep <ip>" | "pin <ip>" | "wait <reason>".
decide() {
  awk -v p="$PREFIX" '
    { if ($4 == 1) { selip = $1; selon = $3 }
      if ($3 == 1 && index($2, p) == 1 && cand == "") cand = $1 }
    END {
      if (selip != "" && selon == 1) { print "keep", selip; exit }
      if (cand == "")               { print "wait", "no-online-exit-node-matching-" p "*"; exit }
      print "pin", cand
    }'
}

# Some networks let STUN (UDP/3478) through to a DERP region but drop its TCP/443,
# so tailscaled picks that region as home by UDP latency, never connects to it, and
# peers' replies (sent to our home DERP) vanish: exit-node tx grows, rx stays 0.
# Dropping our STUN to those regions keeps them from being measured/chosen.
derp_guard() {
  [ -n "$BLOCK_DERP" ] || return 0
  changed=0
  for ip in $(TS debug derp-map 2>/dev/null | awk -v list=" $BLOCK_DERP " '
      /"RegionCode"/ { gsub(/[",]/, "", $2); code = $2 }
      /"IPv4"/       { gsub(/[",]/, "", $2); if (index(list, " " code " ")) print $2 }'); do
    iptables -C OUTPUT -p udp --dport 3478 -d "$ip" -j DROP 2>/dev/null && continue
    iptables -I OUTPUT 1 -p udp --dport 3478 -d "$ip" -j DROP && changed=1
  done
  if [ "$changed" = 1 ]; then
    echo "$(ts) derp-guard: dropping STUN to DERP regions [$BLOCK_DERP]; re-STUN"
    TS debug restun >/dev/null 2>&1; TS debug rebind >/dev/null 2>&1
  fi
}

[ "${1:-}" = "--decide" ] && { decide; exit 0; }     # self-test hook: rows on stdin

last=""; n=0
note() { [ "$1" != "$last" ] && { echo "$(ts) $1"; last="$1"; }; }   # log on change only

echo "$(ts) exit-node-tracker: following '${PREFIX}*' (interval ${INTERVAL}s)"
while :; do
  if [ ! -S "$SOCK" ]; then note "waiting for tailscaled socket"; sleep 5; continue; fi

  [ $((n % 10)) -eq 0 ] && derp_guard; n=$((n + 1))
  set -- $(nodes | decide)
  case "${1:-}" in
    keep) note "ok: exit node $2 selected and online" ;;
    wait) note "waiting: $2" ;;
    pin)
      echo "$(ts) re-pinning exit node -> $2"
      if TS set --exit-node="$2" --exit-node-allow-lan-access; then
        code=000
        for _ in 1 2 3 4; do                      # path to the node can take a few seconds to settle
          sleep 5
          code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 "$PROBE" 2>/dev/null); code=${code:-000}
          case "$code" in 200|204) break ;; esac
        done
        case "$code" in
          200|204) echo "$(ts) pinned $2, egress OK ($code)" ;;
          *)       echo "$(ts) pinned $2, egress probe FAILED ($code) -- will not retry for ${SETTLE}s" ;;
        esac
        last=""
      else
        echo "$(ts) tailscale set --exit-node=$2 FAILED"
      fi
      sleep "$SETTLE" ;;
  esac
  sleep "$INTERVAL"
done

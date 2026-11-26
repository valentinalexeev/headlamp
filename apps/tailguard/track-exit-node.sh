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
#   * if the selected exit node is online and tagged $EXIT_NODE_TAG -> leave it alone
#   * else if an online exit node named $PREFIX* with that tag      -> `tailscale set --exit-node`
#   * else                                                           -> log and wait
#
# The tag check (#390) keeps a node that only copies the name of an exit node from getting the traffic.
# LAN access through the exit node is TG_LAN_ACCESS (0|1, no default): 0 sends the private ranges to the exit
# node too, so a WireGuard peer cannot reach the LAN of the host.
#
# The exit node is NOT passed to tailguard at boot (docker-compose.yml blanks
# TS_EXIT_NODE): env_file values are baked in when the container is created, so
# a stale pin there would make `tailscale up --exit-node=<dead ip>` fail and
# crashloop tailguard after a reboot. This tracker pins it right after boot.
# set -e (#368): the loop must never stop on one failed command, so every command that can fail in normal work
# (tailscale, iptables, curl) is in a condition or has `|| true`.
set -eu

PREFIX="${EXIT_NODE_PREFIX:?set EXIT_NODE_PREFIX (name prefix of the exit node to follow)}"
TAG="${EXIT_NODE_TAG:-tag:exit}"      # only exit nodes with this tag are selected
INTERVAL="${TRACK_INTERVAL:-20}"      # seconds between checks
SETTLE="${TRACK_SETTLE:-45}"          # pause after a re-pin before acting again
SOCK="${TS_SOCKET_PATH:-/tmp/tailscaled.sock}"
PROBE="${TRACK_PROBE:-https://connectivitycheck.gstatic.com/generate_204}"
BLOCK_DERP="${BLOCK_DERP_REGIONS:-}"  # DERP region codes to never pick as home, e.g. "xyz"

if [ "${1:-}" != "--decide" ]; then
  case "${TG_LAN_ACCESS:-}" in
    0) LAN=false ;;
    1) LAN=true ;;
    *) echo "TG_LAN_ACCESS is not set (or not 0|1) in .env (#390). Set it:" >&2
       echo "  TG_LAN_ACCESS=0  WireGuard peers reach the internet through the exit node only, not the LAN of the host" >&2
       echo "  TG_LAN_ACCESS=1  WireGuard peers can also reach the LAN of the host (the behaviour before #390)" >&2
       exit 64 ;;
  esac
fi

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
TS() { tailscale --socket="$SOCK" "$@"; }

# IPv4 addresses of the peers that have $TAG, one a line, from `tailscale status --json` (indented JSON, one
# array element a line; the image has no jq). A peer object starts at its "PublicKey" key.
tagged_ips() {
  { TS status --json 2>/dev/null || true; } | awk -v tag="\"$TAG\"" '
    /"PublicKey":/                    { ip = ""; tagged = 0; done = 0; inips = 0; intags = 0 }
    /"TailscaleIPs": \[/              { inips = 1; next }
    /"Tags": \[/                      { intags = 1; next }
    /^[[:space:]]*\]/                 { inips = 0; intags = 0 }
    inips && ip == "" && /"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+"/ { ip = $1; gsub(/[",]/, "", ip) }
    intags { t = $1; sub(/,$/, "", t); if (t == tag) tagged = 1 }
    ip != "" && tagged && !done       { print ip; done = 1 }'
}

# Exit-node-capable peers as "ip label online selected tagged" (online/selected/tagged 0|1).
# `exit-node list` only prints peers that offer exit node; label = first DNS label.
nodes() {
  tagged="$(tagged_ips | tr '\n' ' ')"
  { TS exit-node list 2>/dev/null || true; } | awk -v tagged=" $tagged" '
    $1 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {
      h = $2; sub(/\..*/, "", h)
      print $1, h, ($0 ~ /offline/ ? 0 : 1), ($0 ~ /selected/ ? 1 : 0), (index(tagged, " " $1 " ") ? 1 : 0)
    }'
}

# stdin: rows from nodes(). stdout: "keep <ip>" | "pin <ip>" | "wait <reason>".
# A selected node that lost the tag is not kept.
decide() {
  awk -v p="$PREFIX" -v t="$TAG" '
    { if ($4 == 1) { selip = $1; selok = ($3 == 1 && $5 == 1) }
      if ($3 == 1 && $5 == 1 && index($2, p) == 1 && cand == "") cand = $1 }
    END {
      if (selip != "" && selok)     { print "keep", selip; exit }
      if (cand == "")               { print "wait", "no-online-exit-node-matching-" p "*-with-" t; exit }
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
  for ip in $({ TS debug derp-map 2>/dev/null || true; } | awk -v list=" $BLOCK_DERP " '
      /"RegionCode"/ { gsub(/[",]/, "", $2); code = $2 }
      /"IPv4"/       { gsub(/[",]/, "", $2); if (index(list, " " code " ")) print $2 }'); do
    if iptables -C OUTPUT -p udp --dport 3478 -d "$ip" -j DROP 2>/dev/null; then continue; fi
    if iptables -I OUTPUT 1 -p udp --dport 3478 -d "$ip" -j DROP; then changed=1; fi
  done
  if [ "$changed" = 1 ]; then
    echo "$(ts) derp-guard: dropping STUN to DERP regions [$BLOCK_DERP]; re-STUN"
    TS debug restun >/dev/null 2>&1 || true; TS debug rebind >/dev/null 2>&1 || true
  fi
}

if [ "${1:-}" = "--decide" ]; then decide; exit 0; fi     # self-test hook: rows on stdin

last=""; n=0
note() { if [ "$1" != "$last" ]; then echo "$(ts) $1"; last="$1"; fi; }   # log on change only

echo "$(ts) exit-node-tracker: following '${PREFIX}*' with ${TAG}, LAN access ${LAN} (interval ${INTERVAL}s)"
while :; do
  if [ ! -S "$SOCK" ]; then note "waiting for tailscaled socket"; sleep 5; continue; fi

  if [ $((n % 10)) -eq 0 ]; then derp_guard; fi; n=$((n + 1))
  # shellcheck disable=SC2046 # word splitting of decide's output into positional args is intended
  set -- $(nodes | decide)
  case "${1:-}" in
    keep) note "ok: exit node $2 selected and online" ;;
    wait) note "waiting: $2" ;;
    pin)
      echo "$(ts) re-pinning exit node -> $2"
      if TS set --exit-node="$2" --exit-node-allow-lan-access="$LAN"; then
        code=000
        for _ in 1 2 3 4; do                      # path to the node can take a few seconds to settle
          sleep 5
          code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 "$PROBE" 2>/dev/null) || true; code=${code:-000}
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

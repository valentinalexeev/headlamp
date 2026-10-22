#!/bin/sh
# Route-health watchdog for tailguard.
#
# Runs as a sidecar container that shares tailguard's network namespace
# (network_mode: "service:tailguard" in docker-compose.yml), so its probes
# egress through the Tailscale exit node exactly like tailguard's own traffic,
# and its iptables edits land in the same namespace tailguard uses.
#
# Behaviour: probe real internet reachability through the exit node. After
# FAIL_N consecutive failures the desired state is "down" -> drop the WireGuard
# listener (udp/$WG_PORT) so clients fail over to their own WAN. After OK_N
# consecutive successes the desired state is "up" -> remove the drop.
#
# The desired state is RE-ASSERTED every cycle (idempotent), not just on
# transitions, so it self-heals if something external clears the rule — e.g.
# tailguard's entrypoint rebuilding its firewall after a host reboot. A startup
# grace period keeps a slow boot (tailscale not up yet) from tripping it.
#
# Fail-open: on exit (compose stop) the listener is always restored.
set -u

WG_PORT="${WG_PORT:-51820}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-6}"  # per-probe curl timeout
INTERVAL="${INTERVAL:-12}"          # seconds between probe cycles
FAIL_N="${FAIL_N:-3}"              # consecutive failures before closing WG
OK_N="${OK_N:-2}"                  # consecutive successes before reopening
GRACE="${GRACE:-150}"             # seconds after start to wait for first success
                                  # before the watchdog is allowed to trip
# Trip time on a black-holed exit node ≈ FAIL_N x (n_probes x PROBE_TIMEOUT +
# INTERVAL) ≈ 90s with defaults (probes time out). Recovery ≈ OK_N x INTERVAL
# ≈ 24s (probes return fast). Tune via env vars in docker-compose.yml.
#
# Probe targets. Egress is OK if ANY returns 200/204, so one target being down
# is not a false positive. Cheap, no-body endpoints.
PROBES="${PROBES:-https://connectivitycheck.gstatic.com/generate_204 http://www.gstatic.com/generate_204 https://1.1.1.1/cdn-cgi/trace}"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

is_blocked() {
  iptables -C INPUT -p udp --dport "$WG_PORT" -j DROP 2>/dev/null
}
block() {
  for ipt in iptables ip6tables; do
    "$ipt" -C INPUT -p udp --dport "$WG_PORT" -j DROP 2>/dev/null \
      || "$ipt" -I INPUT 1 -p udp --dport "$WG_PORT" -j DROP 2>/dev/null || true
  done
}
unblock() {
  # loop: tailguard's healthcheck reshuffles INPUT, and -D removes one match
  for ipt in iptables ip6tables; do
    while "$ipt" -D INPUT -p udp --dport "$WG_PORT" -j DROP 2>/dev/null; do :; done
  done
}

egress_ok() {
  for u in $PROBES; do
    c=$(curl -s -o /dev/null -w '%{http_code}' --max-time "$PROBE_TIMEOUT" "$u" 2>/dev/null || echo 000)
    case "$c" in 200|204) return 0 ;; esac
  done
  return 1
}

trap 'echo "$(ts) route-health: exiting — restoring wg udp/$WG_PORT"; unblock; exit 0' TERM INT

unblock                                   # start from a known-good state
fails=0; oks=0
desired=up                                # what we want the listener to be
armed=0                                   # 0 until the first successful probe / grace expiry
start=$(date +%s)
echo "$(ts) route-health: watching exit-node egress (interval ${INTERVAL}s, timeout ${PROBE_TIMEOUT}s, trip ${FAIL_N}, recover ${OK_N}, grace ${GRACE}s)"

while :; do
  if egress_ok; then
    oks=$((oks + 1)); fails=0
    [ "$armed" -eq 0 ] && { armed=1; echo "$(ts) route-health: armed (egress reachable)"; }
  else
    fails=$((fails + 1)); oks=0
    if [ "$armed" -eq 0 ] && [ $(( $(date +%s) - start )) -ge "$GRACE" ]; then
      armed=1; echo "$(ts) route-health: armed (grace ${GRACE}s elapsed without egress)"
    fi
  fi

  # Decide desired state (only once armed; hysteresis via FAIL_N / OK_N).
  if [ "$armed" -eq 1 ]; then
    if [ "$desired" = up ] && [ "$fails" -ge "$FAIL_N" ]; then
      desired=down; echo "$(ts) EGRESS DOWN (${fails} consecutive) -> closing wg udp/${WG_PORT}"
    elif [ "$desired" = down ] && [ "$oks" -ge "$OK_N" ]; then
      desired=up; echo "$(ts) EGRESS OK (${oks} consecutive) -> reopening wg udp/${WG_PORT}"
    fi
  fi

  # Re-assert desired state every cycle (idempotent) so an external firewall
  # rebuild (e.g. tailguard restart) can't leave us out of sync.
  if [ "$desired" = down ]; then
    is_blocked || { block; echo "$(ts) route-health: re-asserted DROP on wg udp/${WG_PORT}"; }
  else
    ! is_blocked || { unblock; echo "$(ts) route-health: cleared stale DROP on wg udp/${WG_PORT}"; }
  fi

  sleep "$INTERVAL"
done

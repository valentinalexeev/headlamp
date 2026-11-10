#!/bin/sh
# Health of the exit node (#196). It prints an HTTP answer: 200 when every check passes, else 503 with the failed
# checks. start.sh serves it with BusyBox nc on HEALTH_PORT (default 9002), for the Fly check.
#
# Checks:
#   tailscaled   the daemon answers, and is Running
#   exit-node    it advertises 0.0.0.0/0 (the exit route)
#   forwarding   net.ipv4.ip_forward is 1
#   nat          the MASQUERADE rule on eth0 is in place
#   egress       one HTTPS request to HEALTH_EGRESS_URL from this node gets an answer (the uplink of the exit
#                traffic). The last result is cached for HEALTH_EGRESS_TTL seconds (default 60).
# A kernel without xt_connmark is not a fault (see start.sh).
# Test hooks: HEALTH_TAILSCALE (the tailscale binary), HEALTH_PROC (the /proc root), HEALTH_IPTABLES.
ts=${HEALTH_TAILSCALE:-/app/tailscale}
proc=${HEALTH_PROC:-/proc}
ipt=${HEALTH_IPTABLES:-iptables}
url=${HEALTH_EGRESS_URL:-https://www.gstatic.com/generate_204}
ttl=${HEALTH_EGRESS_TTL:-60}
cache=${HEALTH_CACHE:-/tmp/exit-health.egress}

# Read the request line and headers (nc gives us the client connection on stdin), so the client gets the answer.
if [ -z "${HEALTH_NO_REQUEST:-}" ]; then
    while IFS= read -r line; do
        line=$(printf '%s' "$line" | tr -d '\r')
        [ -z "$line" ] && break
    done
fi

fail=""
ok=""
check() { if [ "$2" = 0 ]; then ok="$ok $1"; else fail="$fail $1"; fi; }

status=$("$ts" status --json 2>/dev/null)
case "$status" in *'"BackendState": "Running"'* | *'"BackendState":"Running"'*) r=0 ;; *) r=1 ;; esac
check tailscaled $r
case "$status" in *'"0.0.0.0/0"'*) r=0 ;; *) r=1 ;; esac
check exit-node $r
if [ "$(cat "$proc/sys/net/ipv4/ip_forward" 2>/dev/null)" = 1 ]; then r=0; else r=1; fi
check forwarding $r
if "$ipt" -t nat -S POSTROUTING 2>/dev/null | grep -q -- '-o eth0 -j MASQUERADE'; then r=0; else r=1; fi
check nat $r

now=$(date +%s)
last=$(cat "$cache" 2>/dev/null)
at=${last%% *}
if [ -n "$at" ] && [ $((now - at)) -lt "$ttl" ]; then
    r=${last#* }
else
    wget -q -T 5 --spider "$url" >/dev/null 2>&1
    r=$?
    echo "$now $r" > "$cache"
fi
check egress "$r"

if [ -z "$fail" ]; then
    body="ok:$ok"
    printf 'HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\n' $((${#body} + 1)) "$body"
else
    body="failed:$fail ok:$ok"
    printf 'HTTP/1.0 503 Service Unavailable\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\n' $((${#body} + 1)) "$body"
fi

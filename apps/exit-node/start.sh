#!/usr/bin/env sh
# set -eu (#368): a step that may fail on some kernels or hosts says so and continues (`|| echo ...`); a missing
# setting stops the start with a clear error instead of a login loop with an empty value.
set -eu
: "${HEADSCALE_LOGIN_SERVER:?HEADSCALE_LOGIN_SERVER is required}"
# #394: the auth key is for the first login only (a one-time key). With a valid node state the node logs in without
# it. Copy it to a 0600 file now and drop it from the environment, so no child process and no command line has it.
KEYFILE=""
if [ -n "${TAILSCALE_AUTH_KEY:-}" ]; then
    KEYFILE=$(mktemp /run/ts-authkey.XXXXXX 2>/dev/null || mktemp)
    chmod 600 "$KEYFILE"
    printf '%s' "$TAILSCALE_AUTH_KEY" > "$KEYFILE"
    trap 'rm -f "$KEYFILE"' EXIT
fi
unset TAILSCALE_AUTH_KEY

echo 'Starting up...'

# error: adding [-i tailscale0 -j MARK --set-mark 0x40000] in v4/filter/ts-forward: running [/sbin/iptables -t filter -A ts-forward -i tailscale0 -j MARK --set-mark 0x40000 --wait]: exit status 2: iptables v1.8.6 (legacy): unknown option "--set-mark"
modprobe xt_mark || echo "warning: modprobe xt_mark failed"
# tailscale 1.104 adds connmark rules for rp_filter (mangle PREROUTING/OUTPUT) on every start. The Fly microVM kernel
# has no xt_connmark, so tailscaled logs "router config failed (enabling connmark rules ...)" each time. The NAT and
# filter rules are in place and exit traffic flows: the self-test below proves it (#196). Say so once, as info.
if ! modprobe xt_connmark 2>/dev/null && ! grep -qw xt_connmark /proc/modules 2>/dev/null; then
    echo "info: the kernel has no xt_connmark. tailscaled will log 'router config failed (enabling connmark rules ...)'." \
        "This is expected here: forwarding works without it. See GET :${HEALTH_PORT:-9002}/."
fi

echo 'net.ipv4.ip_forward = 1' | tee -a /etc/sysctl.conf
echo 'net.ipv6.conf.all.forwarding = 1' | tee -a /etc/sysctl.conf
sysctl -p /etc/sysctl.conf || echo "warning: sysctl -p reported an error"
#echo 'net.ipv6.conf.all.disable_policy = 1' | tee -a /etc/sysctl.conf

ethtool -K eth0 rx-udp-gro-forwarding on rx-gro-list off || echo "warning: ethtool could not set UDP GRO forwarding"

iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
ip6tables -t nat -A POSTROUTING -o eth0 -j MASQUERADE || echo "warning: no IPv6 NAT rule"

# --tun=userspace-networking --socks5-server=localhost:1055
# Persistent state (see fly.toml's [mounts]) -- was --state=mem:, which
# meant every redeploy re-registered as a brand new tailnet identity.
STATE_FILE="${STATE_FILE:-/var/lib/tailscale/tailscaled.state}"
export STATE_FILE
mkdir -p "$(dirname "$STATE_FILE")"
# Optional Redis mode (REDIS_URL set): pull the encrypted state from Redis before tailscaled
# starts. With REDIS_URL unset this is a no-op. A failure (e.g. Redis down and no local file)
# aborts startup.
/app/state-sync.sh restore || { echo "state-sync restore failed. exit!"; exit 1; }
TAILSCALED_PID=""
WATCH_PID=""
# Installed right after restore so a stop during boot still pushes. On stop: store the final
# state, stop the watcher, then stop tailscaled. The idle loop backgrounds `sleep` and waits on it so the trap runs promptly. The watcher is not waited on
# (it acts on TERM at once, but the container exits right after anyway).
shutdown() {
    trap '' TERM INT
    /app/state-sync.sh push || echo "final state push failed"
    [ -z "$WATCH_PID" ] || kill "$WATCH_PID" 2>/dev/null || true
    [ -z "$TAILSCALED_PID" ] || kill "$TAILSCALED_PID" 2>/dev/null || true
    exit 0
}
trap shutdown TERM INT

/app/tailscaled --verbose=1 --port 41641 --state="$STATE_FILE" &
TAILSCALED_PID=$!
sleep 5
if [ ! -S /var/run/tailscale/tailscaled.sock ]; then
    echo "tailscaled.sock does not exist. exit!"
    exit 1
fi

# --reset: since state is now persistent (fly.toml [mounts]), the restored
# tailscaled.state already carries prefs from the last run -- notably the
# --advertise-routes set by update-direct-routes.sh below, plus exit-node.
# Without --reset, `tailscale up` here mentions only a subset of non-default
# flags and aborts with "changing settings via 'tailscale up' requires
# mentioning all non-default flags". --reset makes this call authoritative
# for the flags we pass; update-direct-routes.sh re-adds the dynamic routes
# immediately after via `tailscale set` (which has no such all-flags rule).
#
# #394: a node with a valid state logs in without the key. Only a node that needs a login uses it. A key that
# is used or expired does not loop: the node says so once, waits, and the health check answers 503.
backend() { /app/tailscale status --json 2>/dev/null | sed -n 's/.*"BackendState": *"\([A-Za-z]*\)".*/\1/p' | head -1; }
state=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
    state=$(backend) || state=""
    case "$state" in Running|NeedsLogin|Stopped|NeedsMachineAuth) break ;; esac
    sleep 1
done
up() { # up [--auth-key=file:...]
    /app/tailscale up --reset --timeout=60s \
        --login-server="${HEADSCALE_LOGIN_SERVER}" \
        --hostname="${EXIT_NODE_HOSTNAME:-ams}" \
        --advertise-exit-node "$@"
}
logged_in=false
if [ "$state" != NeedsLogin ]; then
    # The state holds a node key: come back with it. A short retry covers a slow control server.
    for _ in 1 2 3 4 5; do
        if up; then logged_in=true; break; fi
        sleep 5
    done
fi
if ! $logged_in && [ "$(backend || true)" = NeedsLogin ]; then
    if [ -z "$KEYFILE" ]; then
        echo "error: the node state has no login and TAILSCALE_AUTH_KEY is not set. Ask an admin for a new one-time" \
            "key, set it as the TAILSCALE_AUTH_KEY secret and restart (#394)."
    elif up --auth-key="file:$KEYFILE"; then
        logged_in=true
    else
        echo "error: the login with TAILSCALE_AUTH_KEY failed: the key is used, expired or not valid for this server." \
            "Ask an admin for a new one-time key, set it as the TAILSCALE_AUTH_KEY secret and restart (#394)."
    fi
fi
[ -z "$KEYFILE" ] || rm -f "$KEYFILE"
if ! $logged_in; then
    # Do not loop on the login (a key would be tried again and again) and do not exit (the platform would restart
    # the container into the same loop). Wait for the operator; the health check answers 503 meanwhile.
    echo "error: the node is not logged in. It waits for a restart with a new key; the health check answers 503."
    if [ "${HEALTH_PORT:-9002}" != off ]; then
        ( while true; do nc -l -p "${HEALTH_PORT:-9002}" -e /app/health.sh; done ) &
    fi
    while :; do sleep 3600 & wait $! || true; done
fi

echo "Tailscale started. Let's go!"

# Health endpoint (#196): GET :HEALTH_PORT/ answers 200 when tailscaled runs as an approved exit node, forwarding
# and NAT are in place, and this node reaches the internet. The Fly check uses it. HEALTH_PORT=off turns it off.
if [ "${HEALTH_PORT:-9002}" != off ]; then
    ( while true; do nc -l -p "${HEALTH_PORT:-9002}" -e /app/health.sh; done ) &
fi

# Push the state file to Redis whenever it changes (no-op when REDIS_URL is unset).
/app/state-sync.sh watch &
WATCH_PID=$!

/app/tailscale set --webclient=true || echo "warning: could not turn on the web client"

# Advertise the direct-hosts routes (Claude, currently) so they route via
# ams independent of any client's exit-node selection -- see
# update-direct-routes.sh. Runs once now, then every 3h in the background
# (matching route-provisioner's route_cron_interval_hours default on the
# VM-based nodes) to pick up IP changes. Not fatal if it fails: ams still
# works fine as a plain exit node either way.
/app/update-direct-routes.sh || echo "update-direct-routes.sh failed on initial run, continuing anyway"
( while true; do
    sleep 3h
    /app/update-direct-routes.sh || echo "update-direct-routes.sh failed, will retry next cycle"
  done ) &

while :; do
    sleep 1 &
    wait $! || true
done

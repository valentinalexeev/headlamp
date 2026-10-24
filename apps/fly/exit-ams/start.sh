#!/usr/bin/env sh

echo 'Starting up...'

# error: adding [-i tailscale0 -j MARK --set-mark 0x40000] in v4/filter/ts-forward: running [/sbin/iptables -t filter -A ts-forward -i tailscale0 -j MARK --set-mark 0x40000 --wait]: exit status 2: iptables v1.8.6 (legacy): unknown option "--set-mark"
modprobe xt_mark

echo 'net.ipv4.ip_forward = 1' | tee -a /etc/sysctl.conf
echo 'net.ipv6.conf.all.forwarding = 1' | tee -a /etc/sysctl.conf
sysctl -p /etc/sysctl.conf
#echo 'net.ipv6.conf.all.disable_policy = 1' | tee -a /etc/sysctl.conf

ethtool -K eth0 rx-udp-gro-forwarding on rx-gro-list off

iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
ip6tables -t nat -A POSTROUTING -o eth0 -j MASQUERADE

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
# (it may only act on TERM after its poll interval); the container exits right after.
shutdown() {
    trap '' TERM INT
    /app/state-sync.sh push || echo "final state push failed"
    [ -z "$WATCH_PID" ] || kill "$WATCH_PID" 2>/dev/null
    [ -z "$TAILSCALED_PID" ] || kill "$TAILSCALED_PID" 2>/dev/null
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
# mentioning all non-default flags", so this until-loop spins forever and the
# node never comes back after a restart. --reset makes this call authoritative
# for the flags we pass; update-direct-routes.sh re-adds the dynamic routes
# immediately after via `tailscale set` (which has no such all-flags rule).
until /app/tailscale up \
    --reset \
    --login-server=${HEADSCALE_LOGIN_SERVER} \
    --authkey=${TAILSCALE_AUTH_KEY} \
    --hostname=ams \
    --advertise-exit-node
#    --advertise-tags=tag:exit
do
    sleep 0.1
done

echo "Tailscale started. Let's go!"

# Push the state file to Redis whenever it changes (no-op when REDIS_URL is unset).
/app/state-sync.sh watch &
WATCH_PID=$!

/app/tailscale set --webclient=true

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
    wait $!
done

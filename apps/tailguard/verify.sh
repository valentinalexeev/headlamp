#!/bin/sh
# Egress proof for the TailGuard WG server: run a throwaway WireGuard client in
# the tunnel, push a download through it, and show the exit-node byte
# counters before/after. Run from apps/tailguard/ on the Docker host, after
# `docker compose up -d`.
#
# Needs config/cli-test.conf — a client config with Endpoint = <host-lan-ip>:51820
# (see README "Home-router client config"; keep DNS/MTU as shown).
set -e
cd "$(dirname "$0")"

CONF=config/cli-test.conf
[ -f "$CONF" ] || { echo "missing $CONF — see README"; exit 1; }

bytes="${1:-20000000}"
img=ghcr.io/juhovh/tailguard:latest

echo "=== exit-node counters BEFORE ==="
docker compose exec -T tailguard tailscale status | grep -E "exit node" || true

work="$(mktemp -d)"
cat > "$work/run.sh" <<EOF
#!/bin/sh
set -e
resolvconf -u 2>/dev/null || true          # else wg-quick's resolvconf -a fails
wg-quick up wg0
sleep 2
echo "[downloading $bytes bytes through the WG tunnel]"
curl -s -o /dev/null -w "  got=%{size_download}B  time=%{time_total}s  speed=%{speed_download}B/s\n" \\
  --max-time 120 "https://speed.cloudflare.com/__down?bytes=$bytes"
echo "[cloudflare trace via tunnel]"
curl -s --max-time 15 https://speed.cloudflare.com/cdn-cgi/trace | grep -E "^ip=|^colo=|^loc="
wg-quick down wg0
EOF
chmod +x "$work/run.sh"

docker run --rm --entrypoint sh --cap-add NET_ADMIN --device /dev/net/tun \
  -v "$PWD/$CONF:/etc/wireguard/wg0.conf" \
  -v "$work/run.sh:/run.sh" \
  "$img" /run.sh
rm -rf "$work"

echo
echo "=== exit-node counters AFTER (rx should have grown by ~$bytes) ==="
docker compose exec -T tailguard tailscale status | grep -E "exit node" || true

echo
echo "=== host baseline cloudflare trace (no tunnel, for comparison) ==="
curl -s --max-time 15 https://speed.cloudflare.com/cdn-cgi/trace | grep -E "^ip=|^colo=|^loc=" || true

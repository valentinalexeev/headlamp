#!/bin/sh
# Grant bridge for the tailnet grants e2e (#139): the main tailscaled (SOCKS 127.0.0.1:1055, tag:ingress), one
# more tailscaled per grant group (SOCKS 127.0.0.1:1056, 1057, ...; each with its own tag key), xray and the agent.
# GROUP_KEYS is "<group id>=<auth key> ..." in port order.
set -eu
mkdir -p /var/lib/ingress-agent /etc/xray /var/run/ts
up() { # socket statedir port hostname key
  tailscaled --tun=userspace-networking --socks5-server="127.0.0.1:$3" --statedir="$2" --socket="$1" --port=0 >>"/var/log/ts-$4.log" 2>&1 &
  i=0; until tailscale --socket="$1" status >/dev/null 2>&1 || tailscale --socket="$1" up --login-server="$LOGIN_SERVER" --authkey="$5" --hostname="$4"; do
    i=$((i + 1)); [ $i -lt 30 ] || exit 1; sleep 1; done
}
up /var/run/ts/main.sock /var/run/ts/main 1055 "$TS_HOSTNAME" "$TS_AUTHKEY"
port=1056
for gk in ${GROUP_KEYS:-}; do
  g=${gk%%=*}; k=${gk#*=}
  up "/var/run/ts/$g.sock" "/var/run/ts/$g" "$port" "$TS_HOSTNAME-$g" "$k"
  port=$((port + 1))
done
cp /etc/xray/config.base.json /etc/xray/config.json
( while true; do xray run -c /etc/xray/config.json >>/var/log/xray.log 2>&1 || true; sleep 0.3; done ) &
printf '#!/bin/sh\npkill -x xray; exit 0\n' > /usr/local/bin/xray-restart; chmod +x /usr/local/bin/xray-restart
exec env XRAY_RESTART_CMD=/usr/local/bin/xray-restart XRAY_BIN=/usr/local/bin/xray AGENT_LISTEN=127.0.0.1:9000 \
  /usr/local/bin/ingress-agent

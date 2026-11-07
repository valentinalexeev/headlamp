#!/bin/sh
# Bridge test entrypoint: tailscaled (userspace, SOCKS5 on 127.0.0.1:1055 through the exit node), xray, the agent.
set -eu
mkdir -p /var/lib/ingress-agent /etc/xray /var/run/ts
tailscaled --tun=userspace-networking --socks5-server=127.0.0.1:1055 --statedir=/var/run/ts >/var/log/tailscaled.log 2>&1 &
i=0; until tailscale status >/dev/null 2>&1 || tailscale up --login-server="$LOGIN_SERVER" --authkey="$TS_AUTHKEY" --hostname="${TS_HOSTNAME:-bridge}"; do
  i=$((i + 1)); [ $i -lt 30 ] || exit 1; sleep 1; done
# xray runs under a small loop: the agent's restart command kills it and the loop starts it again with the new config.
cp /etc/xray/config.base.json /etc/xray/config.json
# "|| true": a killed xray exits non-zero, and under set -e that would end the loop for good.
( while true; do xray run -c /etc/xray/config.json >>/var/log/xray.log 2>&1 || true; sleep 0.3; done ) &
printf '#!/bin/sh\npkill -x xray; exit 0\n' > /usr/local/bin/xray-restart; chmod +x /usr/local/bin/xray-restart
exec env XRAY_RESTART_CMD=/usr/local/bin/xray-restart XRAY_BIN=/usr/local/bin/xray AGENT_LISTEN=127.0.0.1:9000 \
  /usr/local/bin/ingress-agent

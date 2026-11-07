#!/bin/sh
# Bridge test entrypoint: tailscaled (userspace, SOCKS5 on 127.0.0.1:1055 through the exit node), xray, the agent.
set -eu
mkdir -p /var/lib/ingress-agent /etc/xray /var/run/ts
tailscaled --tun=userspace-networking --socks5-server=127.0.0.1:1055 --statedir=/var/run/ts >/var/log/tailscaled.log 2>&1 &
i=0; until tailscale status >/dev/null 2>&1 || tailscale up --login-server="$LOGIN_SERVER" --authkey="$TS_AUTHKEY" --hostname=bridge; do
  i=$((i + 1)); [ $i -lt 30 ] || exit 1; sleep 1; done
# xray runs under a small loop: the agent's restart command kills it and the loop starts it again with the new config.
cp /etc/xray/config.base.json /etc/xray/config.json
( while true; do xray run -c /etc/xray/config.json >>/var/log/xray.log 2>&1; sleep 0.3; done ) &
printf '#!/bin/sh\npkill -x xray; exit 0\n' > /usr/local/bin/xray-restart; chmod +x /usr/local/bin/xray-restart
cd /agent
exec env XRAY_RESTART_CMD=/usr/local/bin/xray-restart XRAY_BIN=/usr/local/bin/xray \
  python3 -m uvicorn app.main:create_app_from_env --factory --host 127.0.0.1 --port 9000 --no-access-log

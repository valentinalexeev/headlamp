#!/bin/sh
# Budget check of the Go ingress agent (#133), on Linux in Docker as on a bridge: the time from the start of the
# process to the first /v1/info answer, the idle RSS, and the RSS after 1,000 credentials are applied. xray is a
# stub that accepts every config, so only the agent is measured.
#   Budget: start < 200 ms, idle RSS <= 15 MB, RSS with 1,000 credentials <= 20 MB.
set -eu
root=$(cd "$(dirname "$0")/../../.." && pwd)
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker is not running"; exit 0; }
img=golang:1.27-alpine
docker run --rm -v "$root/apps/ingress-agent:/src:ro" -w /src -e CGO_ENABLED=0 -e GOFLAGS=-mod=mod "$img" sh -euc '
  go build -trimpath -ldflags="-s -w" -o /tmp/ingress-agent ./cmd/ingress-agent
  apk add -q --no-cache curl python3 >/dev/null
  printf "#!/bin/sh\nexit 0\n" > /tmp/xray; chmod +x /tmp/xray
  mkdir -p /tmp/x && echo "{\"inbounds\":[{\"tag\":\"reality-in\",\"settings\":{\"clients\":[]}}],\"outbounds\":[{\"tag\":\"via-exit\",\"protocol\":\"socks\"}],\"routing\":{\"rules\":[]}}" > /tmp/x/base.json
  tok=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d " \n"); auth="Authorization: Bearer $tok"
  export AGENT_TOKEN="$tok" AGENT_STATE_DIR=/tmp/s XRAY_BIN=/tmp/xray XRAY_BASE_CONFIG=/tmp/x/base.json \
    XRAY_CONFIG=/tmp/x/config.json XRAY_RESTART_CMD=true AGENT_LISTEN=127.0.0.1:9000 \
    INGRESS_NAME=b1 INGRESS_HOST=b1.example.com INGRESS_PUBLIC_KEY=K INGRESS_SHORT_ID=0a
  ms() { python3 -c "import time;print(int(time.monotonic()*1000))"; }  # busybox date has no %N
  t0=$(ms); /tmp/ingress-agent 2>/dev/null & pid=$!
  until curl -sf -H "$auth" http://127.0.0.1:9000/v1/info >/dev/null; do :; done
  t1=$(ms); start_ms=$(( t1 - t0 ))
  sleep 1; idle_kb=$(awk "/VmRSS/{print \$2}" /proc/$pid/status)
  python3 -c "
import json,uuid
print(json.dumps({\"credentials\":[{\"uuid\":str(uuid.uuid4()),\"mode\":[\"auto\",\"direct\",\"exit\"][i%3],\"not_after\":4000000000} for i in range(1000)]}))" > /tmp/b.json
  code=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "$auth" --data-binary @/tmp/b.json http://127.0.0.1:9000/v1/credentials)
  for i in 1 2 3; do curl -s -o /dev/null -X PUT -H "$auth" --data-binary @/tmp/b.json http://127.0.0.1:9000/v1/credentials; done
  sleep 1; full_kb=$(awk "/VmRSS/{print \$2}" /proc/$pid/status)
  size_kb=$(( $(stat -c %s /tmp/ingress-agent) / 1024 ))
  kill $pid
  echo "start_ms=$start_ms idle_rss_kb=$idle_kb rss_1000_kb=$full_kb put_status=$code binary_kb=$size_kb"
  ok=1
  [ "$code" = 200 ] || { echo "not ok - PUT of 1,000 credentials: $code"; ok=0; }
  [ "$start_ms" -lt 200 ] && echo "ok - start ${start_ms} ms < 200 ms" || { echo "not ok - start ${start_ms} ms"; ok=0; }
  [ "$idle_kb" -le 15360 ] && echo "ok - idle RSS ${idle_kb} kB <= 15 MB" || { echo "not ok - idle RSS ${idle_kb} kB"; ok=0; }
  [ "$full_kb" -le 20480 ] && echo "ok - RSS with 1,000 credentials ${full_kb} kB <= 20 MB" || { echo "not ok - RSS ${full_kb} kB"; ok=0; }
  [ $ok = 1 ]
'

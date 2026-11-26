#!/bin/sh
# Budget check of the Go ingress agent (#133), on Linux in Docker as on a bridge: the time from the start of the
# process to the first /v1/info answer, the idle RSS, and the RSS after 1,000 credentials are applied. xray is a
# stub that accepts every config, so only the agent is measured.
# The second run turns the path check on (#426) against a local stub of the control host (central_stub.py: health,
# DERP, STUN and a 1 MiB transfer). It also prints the peak RSS (VmHWM) and the CPU time of the process up to the
# end of the first check, which runs at start and includes the transfer.
#   Budget: start < 200 ms, idle RSS <= 15 MB, RSS with 1,000 credentials <= 20 MB, in both runs.
set -eu
root=$(cd "$(dirname "$0")/../../.." && pwd)
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker is not running"; exit 0; }
img=golang:1.27-alpine
docker run --rm -v "$root/apps/ingress-agent:/src:ro" -v "$root/scripts/test/ingress-agent-budget:/t:ro" -w /src \
  -e CGO_ENABLED=0 -e GOFLAGS=-mod=mod "$img" sh -euc '
  go build -trimpath -ldflags="-s -w" -o /tmp/ingress-agent ./cmd/ingress-agent
  apk add -q --no-cache curl python3 openssl >/dev/null
  printf "#!/bin/sh\nexit 0\n" > /tmp/xray; chmod +x /tmp/xray
  mkdir -p /tmp/x && echo "{\"inbounds\":[{\"tag\":\"reality-in\",\"settings\":{\"clients\":[]}}],\"outbounds\":[{\"tag\":\"via-exit\",\"protocol\":\"socks\"}],\"routing\":{\"rules\":[]}}" > /tmp/x/base.json
  tok=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d " \n"); auth="Authorization: Bearer $tok"
  export AGENT_TOKEN="$tok" XRAY_BIN=/tmp/xray XRAY_BASE_CONFIG=/tmp/x/base.json \
    XRAY_CONFIG=/tmp/x/config.json XRAY_RESTART_CMD=true AGENT_LISTEN=127.0.0.1:9000 \
    INGRESS_NAME=b1 INGRESS_HOST=b1.example.com INGRESS_PUBLIC_KEY=K INGRESS_SHORT_ID=0a
  ms() { python3 -c "import time;print(int(time.monotonic()*1000))"; }  # busybox date has no %N
  cpu_ms() { awk -v hz="$(getconf CLK_TCK 2>/dev/null || echo 100)" "{print int((\$14+\$15)*1000/hz)}" /proc/$1/stat; }
  python3 -c "
import json,uuid
print(json.dumps({\"credentials\":[{\"uuid\":str(uuid.uuid4()),\"mode\":[\"auto\",\"direct\",\"exit\"][i%3],\"not_after\":4000000000} for i in range(1000)]}))" > /tmp/b.json
  ok=1
  # measure LABEL: start the agent with a fresh state dir, check the budget, stop it.
  measure() {
    rm -rf /tmp/s; export AGENT_STATE_DIR=/tmp/s
    t0=$(ms); /tmp/ingress-agent 2>/dev/null & pid=$!
    until curl -sf -H "$auth" http://127.0.0.1:9000/v1/info >/dev/null; do :; done
    t1=$(ms); start_ms=$(( t1 - t0 )); extra=""
    if [ "$1" = pathcheck ]; then
      n=0
      until curl -sf -H "$auth" http://127.0.0.1:9000/v1/pathcheck | grep -q "\"blob\""; do
        n=$(( n + 1 )); [ $n -lt 600 ] || { echo "not ok - $1: no transfer in 60 s"; ok=0; break; }; sleep 0.1
      done
      cpu=$(cpu_ms $pid)
      res=$(curl -s -H "$auth" http://127.0.0.1:9000/v1/pathcheck | python3 -c "
import json,sys
l=json.load(sys.stdin).get(\"last\") or {}; b=l.get(\"blob\") or {}
print(\"class=%s blob_ok=%s blob_bytes=%s\" % (l.get(\"class\"), b.get(\"ok\"), b.get(\"bytes\")))")
      extra=" cpu_first_check_ms=$cpu $res"
      case "$res" in *"blob_ok=True blob_bytes=1048576"*) echo "ok - $1: 1 MiB transfer done";;
        *) echo "not ok - $1: transfer: $res"; ok=0;; esac
    fi
    sleep 1; idle_kb=$(awk "/VmRSS/{print \$2}" /proc/$pid/status); peak_kb=$(awk "/VmHWM/{print \$2}" /proc/$pid/status)
    code=$(curl -s -o /dev/null -w "%{http_code}" -X PUT -H "$auth" --data-binary @/tmp/b.json http://127.0.0.1:9000/v1/credentials)
    for i in 1 2 3; do curl -s -o /dev/null -X PUT -H "$auth" --data-binary @/tmp/b.json http://127.0.0.1:9000/v1/credentials; done
    sleep 1; full_kb=$(awk "/VmRSS/{print \$2}" /proc/$pid/status)
    size_kb=$(( $(stat -c %s /tmp/ingress-agent) / 1024 ))
    kill $pid; wait $pid 2>/dev/null || :
    echo "$1: start_ms=$start_ms idle_rss_kb=$idle_kb peak_rss_kb=$peak_kb rss_1000_kb=$full_kb put_status=$code binary_kb=$size_kb$extra"
    [ "$code" = 200 ] || { echo "not ok - $1: PUT of 1,000 credentials: $code"; ok=0; }
    [ "$start_ms" -lt 200 ] && echo "ok - $1: start ${start_ms} ms < 200 ms" || { echo "not ok - $1: start ${start_ms} ms"; ok=0; }
    [ "$idle_kb" -le 15360 ] && echo "ok - $1: idle RSS ${idle_kb} kB <= 15 MB" || { echo "not ok - $1: idle RSS ${idle_kb} kB"; ok=0; }
    [ "$full_kb" -le 20480 ] && echo "ok - $1: RSS with 1,000 credentials ${full_kb} kB <= 20 MB" || { echo "not ok - $1: RSS ${full_kb} kB"; ok=0; }
  }
  measure off

  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=127.0.0.1 \
    -addext subjectAltName=IP:127.0.0.1 -keyout /tmp/x/key.pem -out /tmp/x/cert.pem 2>/dev/null
  python3 /t/central_stub.py /tmp/x/cert.pem /tmp/x/key.pem 8443 1048576 & stub=$!
  until curl -sf --cacert /tmp/x/cert.pem https://127.0.0.1:8443/health >/dev/null; do sleep 0.1; done
  export INGRESS_PATHCHECK_TARGET=127.0.0.1:8443 INGRESS_PATHCHECK_CA=/tmp/x/cert.pem \
    INGRESS_PATHCHECK_BLOB_URL=https://127.0.0.1:8443/blob INGRESS_PATHCHECK_BLOB_BYTES=1048576
  measure pathcheck
  kill $stub
  [ $ok = 1 ]
'

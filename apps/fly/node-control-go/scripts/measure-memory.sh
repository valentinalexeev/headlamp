#!/bin/sh
# Sample memory of a running node-control Go app over `fly ssh console`, while polling /status.
#
# usage: measure-memory.sh <fly-app> [seconds=300] [status-url]
#
# Every 10 s: RSS of the node-control process (VmRSS) and MemAvailable/MemTotal from /proc/meminfo,
# read inside the machine. If status-url is given (the tailnet URL of the app's /status, for example
# http://node-control.example.com/status) it is requested from this host at each sample, so the UI
# polling load is included; this host must be on the tailnet. Prints min MemAvailable, max RSS and
# PASS/FAIL against: MemAvailable > 20% of MemTotal and RSS < 205 MB.
#
# Read-only: it runs `cat` on /proc files in the machine and GETs one URL. Needs flyctl logged in.
set -u

if [ "$#" -lt 1 ] || [ "$#" -gt 3 ]; then
  echo "usage: $0 <fly-app> [seconds=300] [status-url]" >&2
  exit 2
fi
case $1 in -*) echo "$0: app name must not start with -" >&2; exit 2 ;; esac
app=$1
seconds=${2:-300}
url=${3:-}
case $url in -*) echo "$0: status-url must not start with -" >&2; exit 2 ;; esac
case $seconds in
  '' | *[!0-9]*) echo "$0: seconds must be a number" >&2; exit 2 ;;
esac
command -v fly >/dev/null 2>&1 || { echo "$0: fly (flyctl) is required" >&2; exit 2; }

rss_limit_kb=$((205 * 1024))

# Runs inside the machine: prints "<rss_kb> <avail_kb> <total_kb>". The main process is the one
# whose command line is the node-control binary (PID 1 in the image, but look it up by name).
dir=$(cd "$(dirname "$0")" && pwd)
# probe.sh minus comment lines, passed to the machine as one argument (it holds no single quotes).
probe=$(grep -v '^[[:space:]]*#' "$dir/probe.sh") || exit 2
case $probe in *"'"*) echo "$0: probe.sh must not contain single quotes" >&2; exit 2 ;; esac

min_avail=
max_rss=0
total=0
samples=0
end=$(($(date +%s) + seconds))
while [ "$(date +%s)" -lt "$end" ]; do
  if [ -n "$url" ]; then
    curl -sS --max-time 20 -o /dev/null -- "$url" || echo "warning: GET $url failed" >&2
  fi
  out=$(fly ssh console -a "$app" -C "sh -c '$probe'" 2>&1)
  rc=$?
  case $out in
    *"probe: expected exactly one process"*)
      echo "FAIL: cannot identify the node-control process: $out" >&2
      exit 1 ;;
  esac
  if [ "$rc" -eq 0 ]; then
    pid='' comm='' rss='' avail='' mtotal='' extra=''
    read -r pid comm rss avail mtotal extra <<EOT
$(echo "$out" | tail -n 1)
EOT
    case "$pid$rss$avail$mtotal" in *[!0-9]* | '') ok=0 ;; *) ok=1 ;; esac
    if [ "$ok" = 1 ] && [ -z "$extra" ] && [ -n "$rss" ] && [ -n "$avail" ] && [ -n "$mtotal" ]; then
      total=$mtotal
      samples=$((samples + 1))
      [ "$rss" -gt "$max_rss" ] && max_rss=$rss
      if [ -z "$min_avail" ] || [ "$avail" -lt "$min_avail" ]; then min_avail=$avail; fi
      echo "sample $samples: pid ${pid} (${comm}) RSS ${rss} kB, MemAvailable ${avail} kB of ${mtotal} kB"
    else
      echo "warning: unexpected probe output: $out" >&2
    fi
  else
    echo "warning: fly ssh console failed" >&2
  fi
  sleep 10
done

if [ "$samples" -eq 0 ] || [ "$total" -eq 0 ]; then
  echo "FAIL: no usable samples"
  exit 1
fi
echo "samples: $samples"
echo "min MemAvailable: ${min_avail} kB ($((min_avail * 100 / total))% of ${total} kB)"
echo "max RSS: ${max_rss} kB ($((max_rss / 1024)) MB)"
verdict=PASS
[ $((min_avail * 100)) -gt $((total * 20)) ] || verdict=FAIL
[ "$max_rss" -lt "$rss_limit_kb" ] || verdict=FAIL
echo "$verdict (need MemAvailable > 20% of MemTotal and RSS < 205 MB)"
[ "$verdict" = PASS ]

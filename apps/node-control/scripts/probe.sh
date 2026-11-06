#!/bin/sh
# Runs inside the machine (sent by measure-memory.sh; testable without fly).
# Finds the node-control process by its executable, never by command-line text, and prints
#   <pid> <comm> <rss_kb> <memavailable_kb> <memtotal_kb>
# Exactly one process must run the binary; zero or several is an error (exit 3), never a guess.
# PROC_ROOT and BIN can be overridden for tests. Keep this file free of single quotes.
proc=${PROC_ROOT:-/proc}
bin=${BIN:-/usr/local/bin/node-control}
found=
n=0
for d in "$proc"/[0-9]*; do
  p=${d##*/}
  [ "$p" = "$$" ] && continue
  [ "$p" = "$PPID" ] && continue
  exe=$(readlink "$d/exe" 2>/dev/null) || continue
  if [ "$exe" = "$bin" ]; then
    found=$p
    n=$((n + 1))
  fi
done
if [ "$n" -ne 1 ]; then
  echo "probe: expected exactly one process running $bin, found $n" >&2
  exit 3
fi
comm=$(cat "$proc/$found/comm" 2>/dev/null)
rss=$(awk "/^VmRSS:/ {print \$2}" "$proc/$found/status")
avail=$(awk "/^MemAvailable:/ {print \$2}" "$proc/meminfo")
total=$(awk "/^MemTotal:/ {print \$2}" "$proc/meminfo")
echo "$found ${comm:-?} $rss $avail $total"

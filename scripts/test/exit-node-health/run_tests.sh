#!/bin/sh
# Tests for apps/exit-node/health.sh (#196) with fake tailscale, iptables, /proc and wget. No network.
#   sh scripts/test/exit-node-health/run_tests.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/../../../apps/exit-node/health.sh"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
mkdir -p "$T/bin" "$T/proc/sys/net/ipv4"
fails=0
# Fakes read their answer from files in $T.
cat > "$T/bin/tailscale" <<'S'
#!/bin/sh
cat "$FAKE/status.json"
S
cat > "$T/bin/iptables" <<'S'
#!/bin/sh
cat "$FAKE/nat.txt"
S
cat > "$T/bin/wget" <<'S'
#!/bin/sh
echo call >> "$FAKE/wget.calls"
exit "$(cat "$FAKE/wget.rc")"
S
chmod +x "$T/bin/"*
good() {
  printf '{"BackendState": "Running", "Self": {"AllowedIPs": ["100.64.0.1/32", "0.0.0.0/0", "::/0"]}}' > "$T/status.json"
  echo 1 > "$T/proc/sys/net/ipv4/ip_forward"
  echo '-A POSTROUTING -o eth0 -j MASQUERADE' > "$T/nat.txt"
  echo 0 > "$T/wget.rc"; rm -f "$T/cache" "$T/wget.calls"
}
run() { FAKE=$T PATH="$T/bin:$PATH" HEALTH_TAILSCALE="$T/bin/tailscale" HEALTH_PROC="$T/proc" HEALTH_IPTABLES="$T/bin/iptables" \
  HEALTH_CACHE="$T/cache" HEALTH_NO_REQUEST=1 sh "$script"; }
expect() { # NAME STATUS BODY-SUBSTRING
  out=$(run)
  case "$out" in *"$2"*"$3"*) echo "ok   $1" ;; *) echo "FAIL $1: $(printf '%s' "$out" | tr '\r\n' '  ')"; fails=$((fails + 1)) ;; esac
}

good; expect "all good gives 200" "200 OK" "ok: tailscaled exit-node forwarding nat egress"
good; printf '{"BackendState": "NeedsLogin"}' > "$T/status.json"; expect "not running gives 503" "503" "failed: tailscaled exit-node"
good; printf '{"BackendState": "Running", "Self": {"AllowedIPs": ["100.64.0.1/32"]}}' > "$T/status.json"
expect "no approved exit route gives 503" "503" "failed: exit-node"
good; echo 0 > "$T/proc/sys/net/ipv4/ip_forward"; expect "forwarding off gives 503" "503" "failed: forwarding"
good; : > "$T/nat.txt"; expect "no MASQUERADE gives 503" "503" "failed: nat"
good; echo 4 > "$T/wget.rc"; expect "no internet gives 503" "503" "failed: egress"
# the egress result is cached: a second call within the TTL does not fetch again
good; run >/dev/null; run >/dev/null
[ "$(wc -l < "$T/wget.calls")" -eq 1 ] && echo "ok   egress is cached" || { echo "FAIL egress is cached"; fails=$((fails + 1)); }
# the answer is a complete HTTP response with a matching Content-Length
good; out=$(run)
body=$(printf '%s' "$out" | awk 'f{print} /^\r?$/{f=1}')
len=$(printf '%s' "$out" | sed -n 's/^Content-Length: \([0-9]*\).*/\1/p')
[ "$len" -eq $(( ${#body} + 1 )) ] && echo "ok   Content-Length matches" || { echo "FAIL Content-Length $len vs ${#body}"; fails=$((fails + 1)); }

[ $fails = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

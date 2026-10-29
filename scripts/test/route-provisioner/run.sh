#!/bin/bash
# Tests for the reachability gate in ansible/roles/route-provisioner/templates/update-tailscale-split-routes.sh.j2.
# No network, no root: dig, bgpq4, tailscale, logger and timeout are fakes on PATH, the template's few Jinja
# expressions are filled in with sed, and its two state directories are redirected to a temp dir.
#   bash scripts/test/route-provisioner/run.sh
set -u
here=$(cd "$(dirname "$0")" && pwd)
tpl="$here/../../../ansible/roles/route-provisioner/templates/update-tailscale-split-routes.sh.j2"
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT INT TERM
BIN="$T/bin"; CONF="$T/conf"; WORK="$T/work"; ADV="$T/advertised"
mkdir -p "$BIN" "$CONF" "$WORK"

# dig: A queries answer from $T/dns ("host ip"), cymru TXT queries map every address to AS64500.
cat > "$BIN/dig" <<'STUB'
#!/bin/bash
if [[ "$2" == "TXT" ]]; then echo '"64500 | 192.0.2.0/24 | ZZ | test | 2000-01-01"'; exit 0; fi
awk -v h="$3" '$1 == h {print $2}' "$T_DNS"
STUB
# bgpq4: the whole of AS64500 is 198.51.100.0/24.
printf '#!/bin/bash\necho 198.51.100.0/24\n' > "$BIN/bgpq4"
# tailscale: records the routes it is told to advertise.
printf '#!/bin/bash\nfor a in "$@"; do case "$a" in --advertise-routes=*) echo "${a#*=}" > "$T_ADV";; esac; done\n' > "$BIN/tailscale"
printf '#!/bin/bash\nexit 0\n' > "$BIN/logger"
# timeout: the probed command is `bash -c ... _ IP PORT`; addresses listed in $T_BLOCKED fail, all others connect.
cat > "$BIN/timeout" <<'STUB'
#!/bin/bash
shift
for ip in $T_BLOCKED; do [[ " $* " == *" $ip "* ]] && exit 1; done
exit 0
STUB
chmod +x "$BIN"/*

render() { # $1 = verify true|false
  sed -e "s#/var/lib/tailscale-split-routes#$WORK#" -e "s#/etc/tailscale-split-routes#$CONF#" \
      -e 's#{{ route_min_prefix_v4 }}#8#' -e 's#{{ route_max_prefix_v4 }}#32#' \
      -e "s#{{ 'true' if route_verify_reachability else 'false' }}#$1#" \
      -e 's#{{ route_probe_ports | join(. .) }}#443#' \
      -e 's#{{ route_probe_timeout_seconds }}#1#' -e 's#{{ route_probe_attempts }}#2#' "$tpl" > "$T/script.sh"
}

fails=0
check() { # name, condition result
  if [[ "$2" == ok ]]; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi
}
run() { # args: verify  -> runs the script, output in $T/out, status in $T/rc
  render "$1"
  rm -f "$ADV" "$WORK"/*
  PATH="$BIN:$PATH" T_DNS="$T/dns" T_ADV="$ADV" T_BLOCKED="${BLOCKED:-}" bash "$T/script.sh" > "$T/out" 2>&1
  echo $? > "$T/rc"
}

printf 'good.example\nblocked.example\n' > "$CONF/hosts.txt"
printf 'direct-good.example\ndirect-blocked.example\n' > "$CONF/direct-hosts.txt"
printf 'AS64999\n' > "$CONF/asns.txt"
cat > "$T/dns" <<'DNS'
good.example 192.0.2.10
blocked.example 203.0.113.20
direct-good.example 192.0.2.30
direct-blocked.example 203.0.113.40
DNS

# 1. Gate on: blocked hosts contribute neither their ASN-derived routes nor their /32.
BLOCKED="203.0.113.20 203.0.113.40" run true
check "exits 0" "$([[ $(cat "$T/rc") == 0 ]] && echo ok)"
check "reachable direct host is advertised" "$(grep -q '192.0.2.30/32' "$ADV" && echo ok)"
check "unreachable direct host is not advertised" "$(grep -q '203.0.113.40' "$ADV" && echo no || echo ok)"
check "unreachable host is logged" "$(grep -q 'UNREACHABLE: blocked.example 203.0.113.20' "$T/out" && echo ok)"
check "only reachable hosts feed the ASN list" "$([[ $(cat "$WORK/generated-asns.txt") == AS64500 ]] && echo ok)"
check "summary line" "$(grep -q 'Reachable hosts: 2 of 4' "$T/out" && echo ok)"

# 2. An ASN that only a blocked host sits in is not expanded.
printf 'blocked.example\ngood.example\n' > "$CONF/hosts.txt"
cat > "$BIN/dig" <<'STUB'
#!/bin/bash
if [[ "$2" == "TXT" ]]; then
  case "$3" in 20.113.0.203.*) echo '"64501 | x | ZZ | t | d"';; *) echo '"64500 | x | ZZ | t | d"';; esac; exit 0
fi
awk -v h="$3" '$1 == h {print $2}' "$T_DNS"
STUB
BLOCKED="203.0.113.20" run true
check "ASN of a blocked host is left out" "$([[ $(cat "$WORK/generated-asns.txt") == AS64500 ]] && echo ok)"
BLOCKED="" run true
check "ASN comes back once the host is reachable" "$(grep -q AS64501 "$WORK/generated-asns.txt" && echo ok)"

# 3. An unreachable address inside a prefix that a reachable host pulled in (ASN expansion) is carved out of it.
cat > "$BIN/bgpq4" <<'STUB'
#!/bin/bash
echo 203.0.113.0/24
STUB
BLOCKED="203.0.113.20" run true
check "blocked address is not covered by any advertised prefix" "$(python3 -c "
import ipaddress,sys
nets=[ipaddress.ip_network(x) for x in open('$ADV').read().strip().split(',')]
print('no' if any(ipaddress.ip_address('203.0.113.20') in n for n in nets) else 'ok')")"
check "rest of the prefix is still advertised" "$(python3 -c "
import ipaddress
nets=[ipaddress.ip_network(x) for x in open('$ADV').read().strip().split(',')]
print('ok' if any(ipaddress.ip_address('203.0.113.200') in n for n in nets) else 'no')")"
printf '#!/bin/bash\necho 198.51.100.0/24\n' > "$BIN/bgpq4"

# 3b. Gate off: previous behaviour, everything is published.
BLOCKED="203.0.113.20 203.0.113.40" run false
check "gate off publishes the blocked direct host" "$(grep -q '203.0.113.40/32' "$ADV" && echo ok)"

# 4. Nothing reachable: refuse to touch the advertised routes.
BLOCKED="192.0.2.10 203.0.113.20 192.0.2.30 203.0.113.40" run true
check "all unreachable exits non-zero" "$([[ $(cat "$T/rc") != 0 ]] && echo ok)"
check "all unreachable advertises nothing" "$([[ ! -e "$ADV" ]] && echo ok)"
check "all unreachable says why" "$(grep -q 'Refusing to change Tailscale routes' "$T/out" && echo ok)"

# 5. A host that does not resolve is skipped with a warning.
printf 'nxdomain.example\ngood.example\n' > "$CONF/hosts.txt"
BLOCKED="" run true
check "unresolvable host is warned about" "$(grep -q 'nxdomain.example does not resolve' "$T/out" && echo ok)"

[[ $fails -eq 0 ]] && echo "all passed" || { echo "$fails failed"; exit 1; }

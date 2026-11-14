#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside check(), through eval
# Tests of scripts/ssh-cert-sign.sh with real ssh-keygen and ssh-agent (no network, no hosts).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
sign="$root/scripts/ssh-cert-sign.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

ssh-keygen -q -t ed25519 -N '' -C ca -f "$T/ca"
ssh-keygen -q -t ed25519 -N '' -C hostca -f "$T/hostca"
ssh-keygen -q -t ed25519 -N 'secret' -C locked -f "$T/locked"
ca_fp="$(ssh-keygen -l -f "$T/ca.pub" | awk '{print $2}')"

# 1. Default run: key, certificate, fields.
SSH_CA_KEY="$(cat "$T/ca")" "$sign" --out "$T/o1" --id gha-42 >"$T/o1.log" 2>&1
check "default run exits 0" '[ $? -eq 0 ]'
L="$(ssh-keygen -L -f "$T/o1/id_ed25519-cert.pub" 2>/dev/null)"
check "user certificate" 'grep -q "user certificate" <<<"$L"'
check "signed by the CA" 'grep -q "Signing CA: ED25519 $ca_fp" <<<"$L"'
check "key id" 'grep -q "Key ID: \"gha-42\"" <<<"$L"'
check "default principal deploy" '[ "$(sed -n "/Principals:/,/Critical Options:/p" <<<"$L" | sed "1d;\$d" | tr -d " \t")" = deploy ]'
check "only permit-pty" '[ "$(sed -n "/Extensions:/,\$p" <<<"$L" | sed 1d | tr -d " \t")" = permit-pty ]'
check "no critical options by default" 'sed -n "/Critical Options:/,/Extensions:/p" <<<"$L" | grep -q "(none)"'
from="$(sed -nE 's/.*Valid: from ([^ ]+) to ([^ ]+).*/\1/p' <<<"$L")"
to="$(sed -nE 's/.*Valid: from ([^ ]+) to ([^ ]+).*/\2/p' <<<"$L")"
span="$(python3 -c "import datetime as d,sys;f=d.datetime.fromisoformat;print(int((f(sys.argv[2])-f(sys.argv[1])).total_seconds()))" "$from" "$to")"
check "valid 35 minutes (5 back for clock skew, 30 ahead)" '[ "$span" = 2100 ]'
check "private key mode 600" '[ "$(stat -c %a "$T/o1/id_ed25519" 2>/dev/null || stat -f %Lp "$T/o1/id_ed25519")" = 600 ]'
check "out dir mode 700" '[ "$(stat -c %a "$T/o1" 2>/dev/null || stat -f %Lp "$T/o1")" = 700 ]'
leak=0
for f in "$T/o1"/* "$T/o1.log"; do
  [ "$(ssh-keygen -y -P '' -f "$f" 2>/dev/null | awk '{print $2}')" = "$(awk '{print $2}' "$T/ca.pub")" ] && leak=1
done
check "CA private key not in the output" '[ $leak = 0 ]'
check "no known_hosts without --host-ca-pub" '[ ! -e "$T/o1/known_hosts" ]'

# 2. Options.
SSH_CA_KEY="$(cat "$T/ca")" "$sign" --out "$T/o2" --id gha-43 --principals deploy,ops --validity +2h \
  --source-address 100.64.0.0/10,fd7a:115c:a1e0::/48 --host-ca-pub "$T/hostca.pub" --host-pattern '*.vpn.example.com' >/dev/null 2>&1
check "options run exits 0" '[ $? -eq 0 ]'
L="$(ssh-keygen -L -f "$T/o2/id_ed25519-cert.pub" 2>/dev/null)"
check "two principals" '[ "$(sed -n "/Principals:/,/Critical Options:/p" <<<"$L" | sed "1d;\$d" | tr -d " \t" | paste -sd, -)" = deploy,ops ]'
check "source-address" 'grep -q "source-address 100.64.0.0/10,fd7a:115c:a1e0::/48" <<<"$L"'
check "known_hosts line" '[ "$(cat "$T/o2/known_hosts")" = "@cert-authority *.vpn.example.com $(cat "$T/hostca.pub")" ]'

# 3. Agent mode (SSH_CA_PUB): the CA key sits only in an agent.
eval "$(ssh-agent -s)" >/dev/null
ssh-add -q "$T/ca" 2>/dev/null
SSH_CA_PUB="$T/ca.pub" "$sign" --out "$T/o3" --id op-1 >/dev/null 2>&1
check "agent mode exits 0" '[ $? -eq 0 ]'
check "agent mode signed by the CA" 'ssh-keygen -L -f "$T/o3/id_ed25519-cert.pub" | grep -q "Signing CA: ED25519 $ca_fp"'
kill "$SSH_AGENT_PID" 2>/dev/null; unset SSH_AUTH_SOCK SSH_AGENT_PID

# 4. Refusals.
refuse() { # name, expected message part, args...
  local n="$1" m="$2"; shift 2
  local o; o="$("$@" 2>&1)"; local rc=$?
  if [ $rc -ne 0 ] && grep -q -- "$m" <<<"$o"; then ok "refuses: $n"; else bad "refuses: $n (rc=$rc: $o)"; fi
}
K="$(cat "$T/ca")"
refuse "no CA" "set SSH_CA_KEY" env -u SSH_CA_KEY -u SSH_CA_PUB "$sign" --out "$T/x" --id a
refuse "no --out" "--out is required" env SSH_CA_KEY="$K" "$sign" --id a
refuse "no --id" "--id is required" env SSH_CA_KEY="$K" "$sign" --out "$T/x"
refuse "bad id" "--id: use" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id 'a b'
refuse "bad principals" "--principals" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --principals 'a b'
refuse "validity over 24h" "from +1m to +24h" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --validity +25h
refuse "validity in weeks" "+Nm or +Nh" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --validity +1w
refuse "validity zero" "from +1m to +24h" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --validity +0m
refuse "bad source" "--source-address" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --source-address 'any'
refuse "host CA without pattern" "go together" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --host-ca-pub "$T/hostca.pub"
refuse "key with a passphrase" "not a usable private key" env SSH_CA_KEY="$(cat "$T/locked")" SSH_ASKPASS=/bin/false SSH_ASKPASS_REQUIRE=never "$sign" --out "$T/x" --id a </dev/null
refuse "garbage key" "not a usable private key" env SSH_CA_KEY="not a key" "$sign" --out "$T/x" --id a
refuse "unknown argument" "unknown argument" env SSH_CA_KEY="$K" "$sign" --out "$T/x" --id a --nope

[ "$fails" = 0 ] && echo "all ssh-cert-sign tests passed" || { echo "$fails ssh-cert-sign test(s) failed"; exit 1; }

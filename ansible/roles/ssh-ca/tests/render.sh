#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside expect(), through eval
# Runs the ssh-ca role with real ansible against a throwaway sshd directory on this machine (connection local, no
# root, no service). Checks the files, host certificate signing and renewal, idempotence, and `sshd -t` when sshd is
# installed. The real sshd login test is scripts/test/ssh-ca/e2e-docker.sh.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
roles="$(cd "$here/../.." && pwd)"
command -v ansible-playbook >/dev/null || { echo "SKIP: ansible-playbook is not installed"; exit 0; }
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

sshd_bin="$(command -v sshd || ls /usr/sbin/sshd 2>/dev/null || true)"
validate=false; [ -n "$sshd_bin" ] && validate=true
S="$T/ssh"
mkdir -p "$S/sshd_config.d"
for t in ed25519 ecdsa; do ssh-keygen -q -t "$t" -N '' -C "host-$t" -f "$S/ssh_host_${t}_key"; done
cat > "$S/sshd_config" <<EOF
Include $S/sshd_config.d/*.conf
HostKey $S/ssh_host_ed25519_key
HostKey $S/ssh_host_ecdsa_key
EOF
ssh-keygen -q -t ed25519 -N '' -C userca -f "$T/userca"
ssh-keygen -q -t ed25519 -N '' -C hostca -f "$T/hostca"
ssh-keygen -q -t ed25519 -N '' -C hostca2 -f "$T/hostca2"
mkdir -p "$S/auth_principals"
echo stale > "$S/auth_principals/olduser"

cat > "$T/inv.yml" <<EOF
all:
  hosts:
    node1:
      ansible_connection: local
      ansible_host: node1.example.com
      ansible_python_interpreter: $(command -v python3)
EOF
cat > "$T/play.yml" <<EOF
- hosts: all
  gather_facts: false
  roles: [ssh-ca]
EOF
me="$(id -un)"
run() { # name, extra vars... -> output in $T/<name>.log
  local n="$1"; shift
  ANSIBLE_ROLES_PATH="$roles" ANSIBLE_NOCOLOR=1 ANSIBLE_LOCALHOST_WARNING=0 ANSIBLE_INVENTORY_UNPARSED_WARNING=0 \
    ansible-playbook -i "$T/inv.yml" "$T/play.yml" \
    -e "ssh_ca_sshd_dir=$S" -e ssh_ca_become=false -e ssh_ca_manage_service=false -e "ssh_ca_validate=$validate" \
    -e "ssh_ca_sshd_binary=${sshd_bin:-/nonexistent}" \
    -e "{\"ssh_ca_user_ca_keys\": [\"$(cat "$T/userca.pub")\"]}" \
    -e "{\"ssh_ca_principals\": [{\"user\": \"$me\", \"principals\": [\"deploy\", \"ops\"]}]}" \
    "$@" >"$T/$n.log" 2>&1
}
changed() { sed -nE 's/.*changed=([0-9]+).*/\1/p' "$T/$1.log" | head -n 1; }
expect() { # name, condition
  if eval "$2"; then ok "$1"; else bad "$1"; echo "---- last 30 lines of $3.log"; tail -n 30 "$T/$3.log"; fi
}
# A copy of the role with owner/group dropped, when not root (the real role always writes root-owned files).
if [ "$(id -u)" != 0 ]; then
  mkdir -p "$T/roles"
  cp -R "$roles/ssh-ca" "$T/roles/ssh-ca"
  find "$T/roles/ssh-ca/tasks" -name '*.yml' -exec sed -i.bak -E '/^[[:space:]]+(owner|group): root$/d' {} +
  roles="$T/roles"
fi

hc=(-e ssh_ca_host_certs=true -e "ssh_ca_host_ca_key=$T/hostca" -e '{"ssh_ca_host_key_types": ["ed25519", "ecdsa", "rsa"]}')

# 1. Check mode first: nothing is written.
run check --check "${hc[@]}"; rc=$?
expect "check mode passes" '[ $rc -eq 0 ]' check
expect "check mode writes no drop-in" '[ ! -e "$S/sshd_config.d/40-ssh-ca.conf" ]' check

# 2. First apply.
run apply1 "${hc[@]}"; rc=$?
expect "first apply passes" '[ $rc -eq 0 ]' apply1
expect "CA keys file" '[ "$(cat "$S/ssh_ca_user_keys.pub")" = "$(cat "$T/userca.pub")" ]' apply1
expect "principals file" '[ "$(cat "$S/auth_principals/$me")" = "$(printf "deploy\nops")" ]' apply1
expect "stale principals file removed" '[ ! -e "$S/auth_principals/olduser" ]' apply1
for t in ed25519 ecdsa; do
  L="$(ssh-keygen -L -f "$S/ssh_host_${t}_key-cert.pub" 2>/dev/null)"
  expect "$t host certificate" 'grep -q "host certificate" <<<"$L"' apply1
  expect "$t host certificate names" '[ "$(sed -n "/Principals:/,/Critical Options:/p" <<<"$L" | sed "1d;\$d" | tr -d " \t" | sort | paste -sd, -)" = node1,node1.example.com ]' apply1
  expect "$t host certificate CA" 'grep -q "Signing CA: ED25519 $(ssh-keygen -l -f "$T/hostca.pub" | cut -d" " -f2)" <<<"$L"' apply1
done
expect "no rsa certificate (no rsa host key)" '[ ! -e "$S/ssh_host_rsa_key-cert.pub" ]' apply1
D="$(cat "$S/sshd_config.d/40-ssh-ca.conf" 2>/dev/null)"
expect "drop-in TrustedUserCAKeys" 'grep -qx "TrustedUserCAKeys $S/ssh_ca_user_keys.pub" <<<"$D"' apply1
expect "drop-in AuthorizedPrincipalsFile" 'grep -qx "AuthorizedPrincipalsFile $S/auth_principals/%u" <<<"$D"' apply1
expect "drop-in host certificates" '[ "$(grep -c "^HostCertificate " <<<"$D")" = 2 ]' apply1
if [ "$validate" = true ]; then
  expect "sshd -t accepts the result" '"$sshd_bin" -t -f "$S/sshd_config" 2>"$T/sshd-t.err" || grep -qi "privilege separation\|must be owned by root\|bad owner" "$T/sshd-t.err"' apply1
else
  echo "skip sshd -t (no sshd)"
fi
cert_before="$(cat "$S/ssh_host_ed25519_key-cert.pub")"

# 3. Second apply: nothing changes.
run apply2 "${hc[@]}"
expect "second apply changes nothing" '[ "$(changed apply2)" = 0 ]' apply2
expect "certificate kept" '[ "$(cat "$S/ssh_host_ed25519_key-cert.pub")" = "$cert_before" ]' apply2

# 4. Without a host CA, a due certificate is kept and a warning is printed.
run nosigner -e ssh_ca_host_certs=true -e '{"ssh_ca_host_cert_names": ["node1", "node1.example.com", "extra.example.com"]}'
expect "no signer: passes" 'grep -q "failed=0" "$T/nosigner.log"' nosigner
expect "no signer: warns" 'grep -q "needs signing (other names)" "$T/nosigner.log"' nosigner
expect "no signer: certificate kept" '[ "$(cat "$S/ssh_host_ed25519_key-cert.pub")" = "$cert_before" ]' nosigner

# 5. Another host CA: signed again.
run newca -e ssh_ca_host_certs=true -e "ssh_ca_host_ca_key=$T/hostca2"
expect "new CA: passes" 'grep -q "failed=0" "$T/newca.log"' newca
expect "new CA: signed again" 'ssh-keygen -L -f "$S/ssh_host_ed25519_key-cert.pub" | grep -q "Signing CA: ED25519 $(ssh-keygen -l -f "$T/hostca2.pub" | cut -d" " -f2)"' newca

# 6. Renewal window: with a window longer than the validity, the certificate is due every run.
run renew -e ssh_ca_host_certs=true -e "ssh_ca_host_ca_key=$T/hostca" -e ssh_ca_host_cert_validity=+10d -e ssh_ca_host_cert_renew_days=30
expect "short validity: signed (CA changed back)" 'grep -q "failed=0" "$T/renew.log" && ssh-keygen -L -f "$S/ssh_host_ed25519_key-cert.pub" | grep -q "Signing CA: ED25519 $(ssh-keygen -l -f "$T/hostca.pub" | cut -d" " -f2)"' renew
run renew2 -e ssh_ca_host_certs=true -e "ssh_ca_host_ca_key=$T/hostca" -e ssh_ca_host_cert_validity=+10d -e ssh_ca_host_cert_renew_days=30
expect "due certificate is signed again" 'grep -q "expires in" "$T/renew2.log"' renew2

# 7. Host certificates off: the drop-in names no HostCertificate.
run nocerts
expect "certificates off: no HostCertificate" '! grep -q HostCertificate "$S/sshd_config.d/40-ssh-ca.conf"' nocerts

# 8. Refusals.
run nokeys -e '{"ssh_ca_user_ca_keys": []}'
expect "refuses an empty CA list" 'grep -q "ssh_ca_user_ca_keys must hold" "$T/nokeys.log"' nokeys
run badprinc -e '{"ssh_ca_principals": [{"user": "root", "principals": ["a,b"]}]}'
expect "refuses a principal with a comma" 'grep -q "ssh_ca_user_ca_keys must hold" "$T/badprinc.log"' badprinc
sed -i.bak '/^Include/d' "$S/sshd_config"
run noinclude
expect "refuses a config without Include" 'grep -q "does not include" "$T/noinclude.log"' noinclude

[ "$fails" = 0 ] && echo "all ssh-ca role tests passed" || { echo "$fails ssh-ca role test(s) failed"; exit 1; }

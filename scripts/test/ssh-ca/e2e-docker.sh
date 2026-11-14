#!/usr/bin/env bash
# End-to-end test of the ssh-ca role and scripts/ssh-cert-sign.sh with a real sshd. Run it with `make test-ssh-ca-e2e`.
#
# A throwaway systemd container (the OS matrix image plus openssh-server) gets the role through the docker connection,
# with host certificates signed by a test host CA. Then real ssh logins from this machine check that:
#   a certificate from ssh-cert-sign.sh logs in as root, and the host is trusted through "@cert-authority" only,
#   Ansible runs over ssh with that certificate (the CI path),
#   a wrong principal, a source-address that does not match, a plain key, and a wrong host CA are refused,
#   a second run of the role changes nothing.
# Needs docker, ansible-playbook with community.docker, ssh. Usage: e2e-docker.sh [ubuntu:26.04|debian:13]
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
base="${1:-ubuntu:26.04}"
for c in docker ansible-playbook ssh ssh-keygen; do command -v "$c" >/dev/null || { echo "SKIP: $c is not installed"; exit 0; }; done
sfx=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
host="hlsshca-$sfx"; img="hl-sshca-$sfx"
T="$(mktemp -d)"
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: $host $T"; return; fi
  docker rm -f -v "$host" >/dev/null 2>&1; docker rmi "$img" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
die() { echo "FATAL: $*"; exit 1; }
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

docker build -q -t "$img" --build-arg BASE="$base" --build-arg EXTRA="openssh-server" "$root/ansible/tests/os-matrix" >/dev/null \
  || die "image build"
docker run -d --name "$host" --hostname "$host" --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  --tmpfs /run --tmpfs /run/lock -p 127.0.0.1::22 "$img" >/dev/null || die "host"
for _ in $(seq 60); do
  s="$(docker exec "$host" systemctl is-system-running 2>/dev/null)"; case "$s" in running|degraded) break ;; esac; sleep 1
done
port="$(docker port "$host" 22/tcp | head -n 1 | sed 's/.*://')"
[ -n "$port" ] || die "no published port"
# Test host only: OpenSSH 9.8+ penalises a source address after failed logins (PerSourcePenalties) and then drops its
# connections. The refusal cases below fail on purpose, so without this the later cases would be refused for the wrong
# reason. Each refusal case also checks the reason in the ssh error output.
docker exec "$host" sh -c '/usr/sbin/sshd -T -f /etc/ssh/sshd_config 2>/dev/null | grep -qi "^persourcepenalties" \
  && echo "PerSourcePenalties no" > /etc/ssh/sshd_config.d/10-e2e-test.conf; true'
# Socket-activated or not, sshd must answer before the test starts.
docker exec "$host" sh -c 'systemctl start ssh.socket 2>/dev/null || systemctl start ssh' >/dev/null 2>&1

ssh-keygen -q -t ed25519 -N '' -C userca -f "$T/userca"
ssh-keygen -q -t ed25519 -N '' -C hostca -f "$T/hostca"
ssh-keygen -q -t ed25519 -N '' -C otherca -f "$T/otherca"

printf 'all:\n  hosts:\n    %s:\n      ansible_connection: community.docker.docker\n      ansible_python_interpreter: /usr/bin/python3\n' "$host" > "$T/hosts.yml"
cat > "$T/vars.yml" <<EOF
ssh_ca_user_ca_keys: ["$(cat "$T/userca.pub")"]
ssh_ca_principals: [{user: root, principals: [deploy]}]
ssh_ca_host_certs: true
ssh_ca_host_ca_key: "$T/hostca"
ssh_ca_host_cert_names: ["127.0.0.1", "$host"]
EOF
printf -- '- hosts: all\n  gather_facts: false\n  roles: [ssh-ca]\n' > "$T/play.yml"
apply() {
  ANSIBLE_ROLES_PATH="$root/ansible/roles" ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i "$T/hosts.yml" -e "@$T/vars.yml" "$T/play.yml" >"$T/$1.log" 2>&1
}
if apply apply1; then ok "role applies"; else bad "role applies"; tail -n 30 "$T/apply1.log"; exit 1; fi

# known_hosts with the host CA only: no per-host key line.
sign() { # out-dir, extra args...
  local d="$1"; shift
  SSH_CA_KEY="$(cat "$T/userca")" "$root/scripts/ssh-cert-sign.sh" --out "$d" --id "e2e-$sfx" \
    --host-ca-pub "$T/hostca.pub" --host-pattern "[127.0.0.1]:$port" "$@" >/dev/null 2>&1
}
denied() { grep -q "Permission denied" "$T/ssh.err" || { cat "$T/ssh.err"; false; }; }
login() { # key, known_hosts -> stdout of `id -un`
  ssh -p "$port" -i "$1" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes \
    -o UserKnownHostsFile="$2" -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=10 root@127.0.0.1 id -un 2>"$T/ssh.err"
}

sign "$T/good" || die "sign"
if [ "$(login "$T/good/id_ed25519" "$T/good/known_hosts")" = root ]; then ok "certificate logs in, host trusted by CA"
else bad "certificate logs in, host trusted by CA"; cat "$T/ssh.err"; fi

# The CI path: Ansible over ssh with the certificate.
printf 'all:\n  hosts:\n    target:\n      ansible_host: 127.0.0.1\n      ansible_port: %s\n      ansible_user: root\n      ansible_python_interpreter: /usr/bin/python3\n' "$port" > "$T/ssh-hosts.yml"
if ANSIBLE_PRIVATE_KEY_FILE="$T/good/id_ed25519" ANSIBLE_HOST_KEY_CHECKING=True \
   ANSIBLE_SSH_ARGS="-o UserKnownHostsFile=$T/good/known_hosts -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes -o ControlMaster=no" \
   ansible -i "$T/ssh-hosts.yml" target -m ansible.builtin.ping >"$T/ping.log" 2>&1; then ok "ansible over ssh with the certificate"
else bad "ansible over ssh with the certificate"; tail -n 10 "$T/ping.log"; fi

sign "$T/wrongp" --principals other
if [ -z "$(login "$T/wrongp/id_ed25519" "$T/wrongp/known_hosts")" ] && denied; then ok "wrong principal refused"; else bad "wrong principal refused"; fi

sign "$T/wrongsrc" --source-address 192.0.2.1/32
if [ -z "$(login "$T/wrongsrc/id_ed25519" "$T/wrongsrc/known_hosts")" ] && denied; then ok "other source address refused"; else bad "other source address refused"; fi

ssh-keygen -q -t ed25519 -N '' -f "$T/plain"
if [ -z "$(login "$T/plain" "$T/good/known_hosts")" ] && denied; then ok "plain key refused"
else bad "plain key refused"; fi

printf '@cert-authority [127.0.0.1]:%s %s\n' "$port" "$(cat "$T/otherca.pub")" > "$T/wrong_known_hosts"
if [ -z "$(login "$T/good/id_ed25519" "$T/wrong_known_hosts")" ] && grep -qi "host key verification failed" "$T/ssh.err"; then
  ok "host signed by another CA is not trusted"
else bad "host signed by another CA is not trusted"; cat "$T/ssh.err"; fi

apply apply2
c="$(sed -nE 's/.*changed=([0-9]+).*/\1/p' "$T/apply2.log" | head -n 1)"
if [ "$c" = 0 ]; then ok "second run changes nothing"; else bad "second run changes nothing (changed=$c)"; grep -E '^changed:' "$T/apply2.log"; fi

[ "$fails" = 0 ] && echo "all ssh-ca e2e tests passed ($base)" || { echo "$fails ssh-ca e2e test(s) failed ($base)"; exit 1; }

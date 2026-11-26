#!/usr/bin/env bash
# Checks the preflight of the tailguard role and its templates with real ansible on this machine (connection local, no
# root, no docker): good and bad variables, wg0.conf without a private key and with the preshared keys, .env without
# a key unless the login path asks for it. The real apply runs in the OS matrix (make test-ansible-os ROLES=tailguard).
# Usage: ansible/roles/tailguard/tests/render.sh
set -euo pipefail
if ! command -v ansible-playbook >/dev/null 2>&1; then
  echo "SKIP: ansible-playbook is not installed"
  exit 0
fi
role="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
T="$(mktemp -d)"
trap '[ -n "${KEEP:-}" ] || rm -rf "$T"' EXIT
fails=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }

# Test keys: 32 bytes of base64, not real keys.
pk1="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
pk2="BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA="
psk="CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCA="
# Test join key, made at run time so that the secret scan does not take a literal for a real key.
authkey="test-join-key-$(printf '%s' 0123456789abcdef)"
cat > "$T/good.json" <<EOF
{"tailguard_hostname": "tg-node", "headscale_login_server": "https://hs.example.com",
 "tailguard_exit_node_prefix": "my-exit", "tailguard_lan_access": "0", "tailguard_bind_addr": "192.0.2.10",
 "tailguard_authkey": "$authkey",
 "tailguard_peers": [
   {"name": "router1", "public_key": "$pk1", "allowed_ips": ["10.99.0.2/32", "fd99:7461:6c67::2/128"], "preshared_key": "$psk"},
   {"name": "router2", "public_key": "$pk2", "allowed_ips": ["10.99.0.3/32"]}]}
EOF
cat > "$T/play.yml" <<EOF
- hosts: localhost
  gather_facts: false
  vars_files: ["$role/defaults/main.yml"]
  tasks:
    - ansible.builtin.include_tasks: "$role/tasks/preflight.yml"
    - ansible.builtin.template: {src: "$role/templates/wg0.conf.j2", dest: "$T/wg0.conf"}
      no_log: true
    - ansible.builtin.template: {src: "$role/templates/env.j2", dest: "$T/env"}
    - ansible.builtin.template: {src: "$role/templates/env.j2", dest: "$T/env-login"}
      vars: {tailguard_login_key_file: authkey.abc123}
    - ansible.builtin.template: {src: "$role/templates/docker-compose.yml.j2", dest: "$T/compose.yml"}
EOF
run() { # name, expect(pass|fail), extra -e JSON (merged over good.json)
  local name="$1" expect="$2" extra="${3:-}" got=pass log
  [ -n "$extra" ] || extra="{}"
  # The log file name comes from the check name: keep only safe characters (a "/" in a name made a path).
  log="$T/$(printf '%s' "$name" | tr -c 'A-Za-z0-9._-' '_').log"
  rm -f "$T/wg0.conf" "$T/env" "$T/env-login" "$T/compose.yml"
  ANSIBLE_NOCOLOR=1 ANSIBLE_LOCALHOST_WARNING=0 ANSIBLE_INVENTORY_UNPARSED_WARNING=0 \
    ansible-playbook -i localhost, -c local "$T/play.yml" -e "@$T/good.json" -e "$extra" >"$log" 2>&1 || got=fail
  if [ "$got" != "$expect" ]; then bad "$name (got $got)"; tail -n 20 "$log"; return 1; fi
  if [ "$expect" = fail ] && ! grep -qE "tailguard: .*tailguard_[a-z_]+" "$log"; then
    bad "$name: the message does not name a variable"; tail -n 20 "$log"; return 1
  fi
  if grep -qF "$authkey" "$log" || grep -qF "$psk" "$log"; then bad "$name: a secret is in the log"; return 1; fi
  ok "$name"
}
has() { grep -qE "$2" "$1"; }
# check: run, and go on after a failure (it is counted in fails), so that set -e does not stop the list.
check() { run "$@" || :; }

if run "good variables pass" pass; then
  has "$T/wg0.conf" '^PrivateKey' && bad "wg0.conf has a PrivateKey line" || ok "wg0.conf has no private key"
  has "$T/wg0.conf" '^PostUp = wg set %i private-key /etc/wireguard/server.key$' && ok "wg0.conf loads server.key" || bad "wg0.conf PostUp"
  [ "$(grep -c '^\[Peer\]' "$T/wg0.conf")" = 2 ] && ok "wg0.conf has two peers" || bad "wg0.conf peers"
  [ "$(grep -c '^PresharedKey = ' "$T/wg0.conf")" = 1 ] && ok "only the peer with a preshared key has one" || bad "wg0.conf preshared keys"
  has "$T/wg0.conf" '^AllowedIPs = 10.99.0.2/32, fd99:7461:6c67::2/128$' && ok "wg0.conf allowed IPs" || bad "wg0.conf allowed IPs"
  grep -q 'TS_AUTHKEY' "$T/env" && bad ".env has TS_AUTHKEY" || ok ".env has no TS_AUTHKEY"
  grep -qF "$authkey" "$T/env" "$T/env-login" "$T/compose.yml" && bad "the join key is in a file" || ok "the join key is in no file"
  has "$T/env-login" '^TS_AUTHKEY=file:/tmp/authkey.abc123$' && ok "the login .env has the key file path" || bad "login .env"
  for l in TS_HOSTNAME=tg-node TS_LOGIN_SERVER=https://hs.example.com EXIT_NODE_PREFIX=my-exit 'EXIT_NODE_TAG=tag:exit' \
    TG_LAN_ACCESS=0 TG_STATUS_PORT= TG_BIND_ADDR=192.0.2.10; do
    grep -qxF "$l" "$T/env" && ok ".env $l" || bad ".env $l"
  done
  has "$T/compose.yml" '"192.0.2.10:51820:51820/udp"' && ok "compose binds the WireGuard port to tailguard_bind_addr" || bad "compose port"
  [ "$(grep -c 'image: ghcr.io/valentinalexeev/headlamp/tailguard:' "$T/compose.yml")" = 3 ] && ok "compose uses the image three times" || bad "compose image"
fi
# The role default pins the same image as apps/tailguard/docker-compose.yml.
want="$(sed -n 's/^tailguard_image: //p' "$role/defaults/main.yml")"
grep -qF "image: $want" "$role/../../../apps/tailguard/docker-compose.yml" && ok "the image pin matches apps/tailguard" || bad "image pin differs from apps/tailguard ($want)"

check "missing tailguard_hostname fails" fail '{"tailguard_hostname": ""}'
check "missing login server fails" fail '{"headscale_login_server": ""}'
check "missing tailguard_exit_node_prefix fails" fail '{"tailguard_exit_node_prefix": ""}'
check "missing tailguard_lan_access fails" fail '{"tailguard_lan_access": ""}'
check "tailguard_lan_access 2 fails" fail '{"tailguard_lan_access": "2"}'
check "missing tailguard_bind_addr fails" fail '{"tailguard_bind_addr": ""}'
check "a status port name fails" fail '{"tailguard_status_port": "web"}'
check "status port 8088 passes" pass '{"tailguard_status_port": "8088"}'
check "image latest fails" fail '{"tailguard_image": "ghcr.io/valentinalexeev/headlamp/tailguard:latest"}'
check "image main fails" fail '{"tailguard_image": "ghcr.io/valentinalexeev/headlamp/tailguard:main"}'
check "untagged image fails" fail '{"tailguard_image": "ghcr.io/valentinalexeev/headlamp/tailguard"}'
check "image digest passes" pass "{\"tailguard_image\": \"ghcr.io/valentinalexeev/headlamp/tailguard@sha256:$(printf '%064d' 0)\"}"
check "tailguard_dir / fails" fail '{"tailguard_dir": "/"}'
check "a bad peer public key fails" fail "{\"tailguard_peers\": [{\"name\": \"r\", \"public_key\": \"short\", \"allowed_ips\": [\"10.99.0.2/32\"]}]}"
check "a bad preshared key fails" fail "{\"tailguard_peers\": [{\"name\": \"r\", \"public_key\": \"$pk1\", \"allowed_ips\": [\"10.99.0.2/32\"], \"preshared_key\": \"x$psk\"}]}"
check "a peer without allowed_ips fails" fail "{\"tailguard_peers\": [{\"name\": \"r\", \"public_key\": \"$pk1\", \"allowed_ips\": []}]}"
check "two peers with one key fail" fail "{\"tailguard_peers\": [{\"name\": \"a\", \"public_key\": \"$pk1\", \"allowed_ips\": [\"10.99.0.2/32\"]}, {\"name\": \"b\", \"public_key\": \"$pk1\", \"allowed_ips\": [\"10.99.0.3/32\"]}]}"
check "a peer name with a newline fails" fail "{\"tailguard_peers\": [{\"name\": \"a\\nb\", \"public_key\": \"$pk1\", \"allowed_ips\": [\"10.99.0.2/32\"]}]}"

[ "$fails" = 0 ] && echo "all checks passed" || { echo "$fails check(s) failed"; exit 1; }

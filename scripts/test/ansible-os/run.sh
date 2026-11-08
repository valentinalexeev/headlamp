#!/usr/bin/env bash
# OS matrix for the Ansible roles (#170). Runs every role in check mode and then for real against a throwaway systemd
# container that stands for one target image, with a real headscale in a second container.
# Usage: scripts/test/ansible-os/run.sh <ubuntu-minimal|ubuntu-server|debian13> [role ...]
# Needs docker (linux/amd64, emulated on arm64), ansible-playbook with community.docker and ansible.posix, go, curl.
# APPLY="<role> ..." limits the real apply to these roles (default: all); the other roles run in check mode only.
# E2E_KEEP=1 keeps the containers. Prints one "RESULT <os> <role> <check> <apply>" line for each role.
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
os="${1:?usage: run.sh <ubuntu-minimal|ubuntu-server|debian13> [role ...]}"; shift
case "$os" in
  ubuntu-minimal) base=ubuntu:26.04; extra="" ;;
  # The package set of the Server image on top of the minimal base.
  ubuntu-server) base=ubuntu:26.04; extra="ubuntu-server-minimal" ;;
  debian13) base=debian:13; extra="" ;;
  *) echo "unknown OS '$os'"; exit 2 ;;
esac
roles=("$@")
[ $# -gt 0 ] || roles=(tailscale route-provisioner tailscale-userspace-socks reality-ingress ingress-agent task-runner)
for c in docker ansible-playbook go curl python3; do command -v "$c" >/dev/null || { echo "SKIP: $c is not installed"; exit 0; }; done

sfx=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
net="hlos-$sfx"; hs="hlos-hs-$sfx"; host="hlos-$os-$sfx"; img="hl-os-$os"
T="$(mktemp -d)"
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: $host $hs $T"; return; fi
  docker rm -f -v "$host" "$hs" >/dev/null 2>&1; docker network rm "$net" >/dev/null 2>&1; rm -rf "$T"
  # The image is large; KEEP_IMAGE=1 keeps it for the next run.
  [ "${KEEP_IMAGE:-}" = 1 ] || docker rmi "$img" >/dev/null 2>&1
}
trap cleanup EXIT
die() { echo "FATAL: $*"; exit 1; }

docker build -q --platform linux/amd64 -t "$img" --build-arg BASE="$base" --build-arg EXTRA="$extra" "$root/ansible/tests/os-matrix" >/dev/null \
  || die "image build"
docker network create "$net" >/dev/null || die "network"

# headscale over plain HTTP on the test network. Tagged keys: tag:exit (the node of the tailscale role, auto-approved as
# an exit node) and tag:socks (the second node of tailscale-userspace-socks).
hsv="$(tr -d '[:space:]' < "$root/HEADSCALE_VERSION")"
mkdir -p "$T/hs"
cat > "$T/hs/config.yaml" <<YAML
server_url: http://$hs:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: true
    region_id: 999
    region_code: t
    region_name: t
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /var/lib/headscale/derp.key
    automatically_add_embedded_derp_region: true
  urls: []
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /var/lib/headscale/db.sqlite
dns:
  magic_dns: false
  override_local_dns: false
policy:
  mode: database
unix_socket: /var/lib/headscale/hs.sock
log:
  level: warn
YAML
chmod 644 "$T/hs/config.yaml"
docker run -d --name "$hs" --network "$net" -v "$T/hs:/etc/headscale:ro" "ghcr.io/juanfont/headscale:$hsv" serve >/dev/null || die "headscale"
hx() { docker exec -i "$hs" headscale "$@"; }
for _ in $(seq 40); do hx users list >/dev/null 2>&1 && break; sleep 1; done
hx users create svc >/dev/null || die "headscale not ready"
printf '%s' '{"tagOwners":{"tag:exit":["svc@"],"tag:socks":["svc@"]},"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}],"autoApprovers":{"exitNode":["tag:exit"]}}' \
  | hx policy set -f /dev/stdin >/dev/null || die "policy"
svc_id="$(hx users list -o json | python3 -c 'import json,sys;print([u["id"] for u in json.load(sys.stdin) if u["name"]=="svc"][0])')"
key() { hx preauthkeys create -u "$svc_id" -e 1h --tags "$1" -o json | python3 -c 'import json,sys;print(json.load(sys.stdin)["key"])'; }
k_exit="$(key tag:exit)"; k_socks="$(key tag:socks)"

# The host: systemd as PID 1. Privileged for /dev/net/tun, nftables and the nested rootless podman.
docker run -d --platform linux/amd64 --name "$host" --hostname "$host" --network "$net" --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock "$img" >/dev/null || die "host"
for _ in $(seq 60); do
  s="$(docker exec "$host" systemctl is-system-running 2>/dev/null)"; case "$s" in running|degraded) break ;; esac; sleep 1
done

# reality-ingress uses its default pins: the official xray and caddy images by digest, with the sha256 of each file.
cat > "$T/vars.yml" <<YAML
tailscale_version: "$(tr -d '[:space:]' < "$root/TAILSCALE_VERSION")"
headscale_login_server: "http://$hs:8080"
tailscale_authkey: "$k_exit"
tailscale_hostname: "exit-$os"
route_hosts_direct: [example.com]
socks_hostname: "socks-$os"
socks_exit_node: "exit-$os"
socks_authkey: "$k_socks"
ingress_host: relay.example.com
ingress_name: ingress-1
reality_private_key: "$(python3 -c 'import base64,os;print(base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("="))')"
reality_public_key: fakePublicKey_for-tests
reality_short_id: 0123abcd
enroll_upstream: "127.0.0.1:8081"
hy2_enabled: false
# Docker gives the container its embedded resolver.
ingress_guard_resolvers: ["127.0.0.11", "127.0.0.53"]
ingress_agent_src: "$root/apps/ingress-agent"
ingress_agent_token: "$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
task_runner_src: "$root/apps/task-runner"
task_runner_token: "$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
task_runner_image_archive: "$T/run-image.tar"
task_runner_guard_resolvers: ["127.0.0.11", "100.100.100.100", "fd7a:115c:a1e0::53"]
task_runner_min_free_mb: 64
task_runner_min_free_disk_mb: 64
YAML
# A tiny stand-in run image: the matrix checks the install, not Chromium.
docker pull -q --platform linux/amd64 busybox:1.37 >/dev/null && docker save -o "$T/run-image.tar" busybox:1.37 || die "run image"

# The connection settings go in the inventory, so tasks delegated to localhost keep the local connection.
printf 'all:\n  hosts:\n    %s:\n      ansible_connection: community.docker.docker\n      ansible_python_interpreter: /usr/bin/python3\n' "$host" > "$T/hosts.yml"
# Check mode of the apt module needs python3-apt, which a minimal image does not have (README "Supported systems").
docker exec "$host" sh -c 'apt-get update -qq && apt-get install -y -qq --no-install-recommends python3-apt' >/dev/null 2>&1 || die "python3-apt"

play() { # role mode(check|apply) -> rc
  local role="$1" mode="$2" extra=()
  [ "$mode" = check ] && extra=(--check)
  # task-runner binds to the tailnet address, which only exists after the tailscale role.
  if [ "$role" = task-runner ]; then
    local ip; ip="$(docker exec "$host" tailscale ip -4 2>/dev/null | grep -E '^100[.]' | head -n 1)"
    extra+=(-e "task_runner_listen_addr=${ip:-100.64.0.250}")
  fi
  cat > "$T/play-$role.yml" <<EOF2
- hosts: all
  gather_facts: true
  roles: [$role]
EOF2
  ANSIBLE_ROLES_PATH="$root/ansible/roles" ANSIBLE_HOST_KEY_CHECKING=False ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i "$T/hosts.yml" -e "@$T/vars.yml" ${extra[@]+"${extra[@]}"} "$T/play-$role.yml" >"$T/$role-$mode.log" 2>&1
}

# A role passes "apply" when the play passes, and a second run changes nothing that is not expected to change.
fails=0
for r in "${roles[@]}"; do
  c=fail; a=fail
  play "$r" check && c=ok
  if [ -n "${APPLY:-}" ] && ! printf ' %s ' "$APPLY" | grep -q " $r "; then
    a=skip
  elif play "$r" apply; then
    a=ok
    case "$r" in
      tailscale) docker exec "$host" tailscale status --json | grep -q '"BackendState": *"Running"' || a=fail ;;
      tailscale-userspace-socks) docker exec "$host" systemctl is-active --quiet tailscaled-socks || a=fail ;;
      reality-ingress) for u in xray caddy-ingress ingress-guard; do docker exec "$host" systemctl is-active --quiet "$u" || a=fail; done ;;
      ingress-agent) docker exec "$host" systemctl is-active --quiet ingress-agent || a=fail ;;
      task-runner) docker exec "$host" systemctl is-active --quiet task-runner || a=fail ;;
      route-provisioner) docker exec "$host" sh -c 'command -v bgpq4 && command -v dig' >/dev/null || a=fail ;;
    esac
  fi
  for m in check apply; do
    v=$c; [ $m = apply ] && v=$a
    case "$v" in ok|skip) continue ;; esac; { echo "---- $r $m (last 25 lines)"; tail -n 25 "$T/$r-$m.log"; }
  done
  echo "RESULT $os $r check=$c apply=$a"
  case "$c$a" in okok|okskip) ;; *) fails=$((fails + 1)) ;; esac

done
[ $fails = 0 ] && echo "all roles passed on $os" || { echo "$fails role(s) failed on $os"; exit 1; }

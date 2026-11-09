#!/usr/bin/env bash
# End-to-end test of the exit-node-vm/aws user data with SSM state (#213), on real systemd hosts. Run it with
# `make test-ssm-state-vm-e2e`. It renders the user data of the module (tailscale_version + state_parameter_name +
# tailscale_authkey), and on each throwaway Ubuntu systemd host it lays down write_files and runs runcmd, as cloud-init
# does. Host 1 joins. It is removed, and host 2 (same user data, a new machine) starts. Checks: tailscaled runs with the
# SSM --state from the drop-in, the package install does not stop at a conffile prompt, host 2 skips the key, and
# headscale keeps one node with the same id, name and IPv4.
#   E2E_PLATFORM default linux/amd64 (the pinned .deb repo has both; amd64 matches the published images)
#   E2E_KEEP=1   keep the containers
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ls_image=${E2E_LOCALSTACK_IMAGE:-localstack/localstack:4}
platform=${E2E_PLATFORM:-linux/amd64}
tsv="$(tr -d '[:space:]' < "$root/TAILSCALE_VERSION")"

skip() { echo "SKIP: $*"; exit 0; }
for c in docker jq terraform python3; do command -v "$c" >/dev/null 2>&1 || skip "$c is not installed"; done
docker info >/dev/null 2>&1 || skip "docker is not running"

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="ssmvm-$sfx"; hs="ssmvm-hs-$sfx"; ls="ssmvm-ls-$sfx"; h1="ssmvm-h1-$sfx"; h2="ssmvm-h2-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers ssmvm-*-$sfx, dir $T"; return; fi
  for c in "$h2" "$h1" "$ls" "$hs"; do docker rm -f "$c" >/dev/null 2>&1; done
  docker network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$h1" "$h2"; do echo "--- $c"; docker exec "$c" journalctl -u tailscaled --no-pager -n 15 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

echo "# images"
docker build -q --platform "$platform" -t ssmvm-host "$root/ansible/tests/os-matrix" >/dev/null || die "host image"
for i in "$hs_image" "$ls_image"; do docker image inspect "$i" >/dev/null 2>&1 || docker pull -q "$i" >/dev/null || die "pull $i"; done

mkdir -p "$T/hs"
cat > "$T/hs/config.yaml" <<YAML
server_url: http://$hs
listen_addr: 0.0.0.0:80
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: true
noise: { private_key_path: /data/noise_private.key }
prefixes: { v4: 100.64.0.0/10, v6: fd7a:115c:a1e0::/48 }
derp:
  server: { enabled: true, region_id: 999, region_code: e2e, region_name: e2e, stun_listen_addr: 0.0.0.0:3478, private_key_path: /data/derp.key, automatically_add_embedded_derp_region: true }
  urls: []
  auto_update_enabled: false
database: { type: sqlite, sqlite: { path: /data/db.sqlite } }
dns: { magic_dns: false, override_local_dns: false }
unix_socket: /data/headscale.sock
YAML
docker network create "$net" >/dev/null || die network
docker run -d --name "$hs" --network "$net" -v "$T/hs:/etc/headscale:ro" "$hs_image" serve >/dev/null || die headscale
docker run -d --name "$ls" --network "$net" -e SERVICES=ssm "$ls_image" >/dev/null || die localstack
hsc() { docker exec "$hs" headscale "$@"; }
wait_for 60 hsc users list || die "headscale"
wait_for 90 sh -c "docker exec $ls curl -sf http://localhost:4566/_localstack/health | grep -q '\"ssm\": \"\\(available\\|running\\)\"'" || die "localstack"
hsc users create exit >/dev/null
uid=$(hsc users list -o json | jq -r '.[] | select(.name=="exit") | .id')
key=$(hsc preauthkeys create -u "$uid" --reusable --expiration 1h -o json | jq -r .key)

echo "# render the module's user data"
mod="$root/terraform/modules/exit-node-vm/aws"
mkdir -p "$T/r"
cat > "$T/r/main.tf" <<EOF
output "ud" {
  sensitive = true
  value = templatefile("$mod/templates/cloud-init-v2.yaml.tmpl", {
    headscale_login_server = "http://$hs", tailscale_authkey = "$key", authkey_secret_arn = null,
    tailscale_version = "$tsv", hostname = "cobalt", fetch_secret_b64 = "",
    state_arn = "arn:aws:ssm:eu-central-1:000000000000:parameter/headlamp/cobalt/tailscaled" })
}
EOF
(cd "$T/r" && terraform init -no-color >/dev/null && terraform apply -auto-approve -no-color >/dev/null && terraform output -raw ud) > "$T/ud.yaml" || die render
grep -q -- "--state=arn:aws:ssm:eu-central-1:000000000000:parameter/headlamp/cobalt/tailscaled --socket" "$T/ud.yaml" && ok "user data has the SSM drop-in" || nok "no SSM drop-in in the user data"
# Test only: the emulated host has no kernel tun or iptables, so the drop-in also gets userspace networking.
sed -i.bak 's|^\(      ExecStart=/usr/sbin/tailscaled --state=arn\)|      ExecStart=/usr/sbin/tailscaled --tun=userspace-networking --state=arn|' "$T/ud.yaml"

# boot <host>: a fresh systemd host; then cloud-init's work: write_files, then each runcmd entry in order.
boot() {
  local h="$1"
  docker run -d --platform "$platform" --name "$h" --hostname "$h" --network "$net" --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock ssmvm-host >/dev/null || die "start $h"
  wait_for 60 docker exec "$h" systemctl is-system-running --wait || true
  # The instance role would give the AWS env; here it points the SDK of tailscaled at LocalStack. 
  docker exec "$h" sh -c "mkdir -p /etc/systemd/system/tailscaled.service.d && printf '[Service]\nEnvironment=AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_EC2_METADATA_DISABLED=true AWS_ENDPOINT_URL_SSM=http://$ls:4566\n' > /etc/systemd/system/tailscaled.service.d/00-e2e-aws.conf"
  docker exec "$h" sh -c 'apt-get update -qq >/dev/null && apt-get install -y -qq curl python3-yaml >/dev/null 2>&1' || die "base packages on $h"
  docker cp "$T/ud.yaml" "$h:/root/ud.yaml"
  docker exec -i "$h" python3 - <<'PY' || die "cloud-init on $h"
import os, subprocess, sys, yaml
d = yaml.safe_load(open("/root/ud.yaml"))
for f in d.get("write_files", []):
    os.makedirs(os.path.dirname(f["path"]), exist_ok=True)
    open(f["path"], "w").write(f["content"])
    if "permissions" in f: os.chmod(f["path"], int(f["permissions"], 8))
subprocess.run(["systemctl", "daemon-reload"], check=True)
for i, c in enumerate(d["runcmd"]):
    r = subprocess.run(["sh", "-c", c] if isinstance(c, str) else c, capture_output=True, text=True, stdin=subprocess.DEVNULL)
    open(f"/root/runcmd-{i}.log", "w").write(r.stdout + r.stderr)
    if r.returncode != 0:
        print(f"runcmd {i} rc={r.returncode}: {(r.stdout + r.stderr)[-600:]}", file=sys.stderr); sys.exit(1)
PY
  docker exec "$h" sh -c 'ls /root/runcmd-0.log' >/dev/null 2>&1 || die "runcmd did not run on $h"
}
ts_state() { docker exec "$1" tailscale status --json 2>/dev/null | jq -r .BackendState; }

echo "# 1. host 1 boots: install, then join with the key"
boot "$h1"
docker exec "$h1" sh -c 'tr "\0" " " < /proc/$(systemctl show -p MainPID --value tailscaled)/cmdline' | grep -q -- "--state=arn:aws:ssm:eu-central-1:000000000000:parameter/headlamp/cobalt/tailscaled" \
  && ok "tailscaled runs with the SSM --state from the drop-in" || nok "tailscaled does not use the SSM state"
[ "$(docker exec "$h1" dpkg-query -W -f '${Version}' tailscale)" = "$tsv" ] && ok "tailscale $tsv installed with no conffile prompt" || nok "tailscale not installed at $tsv"
docker exec "$h1" grep -q 'FLAGS=""' /etc/default/tailscaled && ok "/etc/default/tailscaled is the package copy" || nok "/etc/default/tailscaled changed"
wait_for 30 sh -c "[ \"\$(docker exec $h1 tailscale status --json | jq -r .BackendState)\" = Running ]" && ok "host 1 is Running" || nok "host 1 is $(ts_state "$h1")"
n1=$(hsc nodes list -o json); id1=$(jq -r '.[0].id' <<<"$n1"); nm1=$(jq -r '.[0].given_name' <<<"$n1"); ip1=$(jq -r '.[0].ip_addresses[0]' <<<"$n1")
[ "$(jq length <<<"$n1")" = 1 ] && ok "one node (id $id1, $nm1, $ip1)" || nok "$(jq length <<<"$n1") nodes after host 1"
docker exec "$h1" test -e /var/lib/tailscale/tailscaled.state && nok "a state file is on the disk" || ok "no state file on the disk"

echo "# 2. host 1 is gone (scale to 0); host 2 boots from the same user data"
docker rm -f "$h1" >/dev/null
boot "$h2"
docker exec "$h2" sh -c 'cat /root/runcmd-*.log' | grep -q "invalid key\|authkey" && nok "host 2 used the key" || true
wait_for 30 sh -c "[ \"\$(docker exec $h2 tailscale status --json | jq -r .BackendState)\" = Running ]" && ok "host 2 is Running" || nok "host 2 is $(ts_state "$h2")"
n2=$(hsc nodes list -o json)
[ "$(jq length <<<"$n2")" = 1 ] && ok "still one node" || nok "$(jq length <<<"$n2") nodes after host 2: $(jq -c '[.[]|.given_name]' <<<"$n2")"
[ "$(jq -r '.[0].id' <<<"$n2")" = "$id1" ] && ok "same id" || nok "id is $(jq -r '.[0].id' <<<"$n2")"
[ "$(jq -r '.[0].given_name' <<<"$n2")" = "$nm1" ] && ok "same given name" || nok "given name is $(jq -r '.[0].given_name' <<<"$n2")"
[ "$(jq -r '.[0].ip_addresses[0]' <<<"$n2")" = "$ip1" ] && ok "same IPv4" || nok "IPv4 is $(jq -r '.[0].ip_addresses[0]' <<<"$n2")"
[ "$(jq -r '.[0].online' <<<"$n2")" = true ] && ok "online" || nok "not online"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ]

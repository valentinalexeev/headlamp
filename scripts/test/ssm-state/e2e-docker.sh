#!/usr/bin/env bash
# End-to-end test of #213: tailscaled keeps its state in AWS SSM Parameter Store, so a new instance with the same
# parameter is the same headscale node. Run it with `make test-ssm-state-e2e`.
#
# It runs a real headscale, LocalStack (SSM only) and tailscale clients. Client 1 joins with an auth key and
# --state=arn:aws:ssm:...; its container is then removed (what a scale to 0 does to an instance). Client 2 is a NEW
# container with the same --state and no auth key. The test checks that headscale still has one node, with the same
# id, given name and IPv4, and that the state parameter is a SecureString. A third client with a plain file state and
# the same reusable key shows the old behaviour: a second node.
#
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
#   E2E_TAILSCALE_IMAGE  default tailscale/tailscale:v1.104.4
#   E2E_LOCALSTACK_IMAGE default localstack/localstack:4
#   E2E_KEEP=1           keep the containers
set -uo pipefail
hs_image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
ts_image=${E2E_TAILSCALE_IMAGE:-tailscale/tailscale:v1.104.4}
ls_image=${E2E_LOCALSTACK_IMAGE:-localstack/localstack:4}

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker info >/dev/null 2>&1 || skip "docker is not running"
command -v jq >/dev/null 2>&1 || skip "jq is not installed"

tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 120 docker "$@"; }

sfx=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
net="ssm-e2e-$sfx"
hs="ssm-e2e-hs-$sfx"; ls="ssm-e2e-ls-$sfx"; c1="ssm-e2e-c1-$sfx"; c2="ssm-e2e-c2-$sfx"; c3="ssm-e2e-c3-$sfx"; c4="ssm-e2e-c4-$sfx"
T=$(mktemp -d)
cleanup() {
  if [ "${E2E_KEEP:-}" = 1 ]; then echo "kept: network $net, containers ssm-e2e-*-$sfx"; return; fi
  for c in "$c4" "$c3" "$c2" "$c1" "$ls" "$hs"; do dk rm -f "$c" >/dev/null 2>&1; done
  dk network rm "$net" >/dev/null 2>&1; rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
die() { echo "FATAL: $1"; for c in "$hs" "$ls" "$c1" "$c2" "$c3" "$c4"; do echo "--- logs $c"; docker logs --tail 20 "$c" 2>&1; done; exit 1; }
wait_for() { n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do "$@" >/dev/null 2>&1 && return 0; i=$((i + 1)); sleep 1; done; return 1; }

for i in "$hs_image" "$ts_image" "$ls_image"; do
  dk image inspect "$i" >/dev/null 2>&1 || tmo 300 docker pull -q "$i" >/dev/null || die "pull $i"
done

mkdir -p "$T/etc"
cat > "$T/etc/config.yaml" <<YAML
server_url: http://$hs
listen_addr: 0.0.0.0:80
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: true
noise:
  private_key_path: /data/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: true
    region_id: 999
    region_code: e2e
    region_name: e2e
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /data/derp_server_private.key
    automatically_add_embedded_derp_region: true
  urls: []
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /data/db.sqlite
dns:
  magic_dns: false
  override_local_dns: false
unix_socket: /data/headscale.sock
YAML

dk network create "$net" >/dev/null || die "network"
dk run -d --name "$hs" --network "$net" -v "$T/etc:/etc/headscale:ro" "$hs_image" serve >/dev/null || die "headscale"
dk run -d --name "$ls" --network "$net" -e SERVICES=ssm "$ls_image" >/dev/null || die "localstack"
hsc() { dk exec "$hs" headscale "$@"; }
wait_for 60 hsc users list || die "headscale did not start"
wait_for 90 sh -c "docker exec $ls curl -sf http://localhost:4566/_localstack/health | grep -q '\"ssm\": \"\\(available\\|running\\)\"'" || die "localstack ssm did not start"
hsc users create exit >/dev/null || die "user"
uid=$(hsc users list -o json | jq -r '.[] | select(.name=="exit") | .id')
key=$(hsc preauthkeys create -u "$uid" --reusable --expiration 1h -o json | jq -r '.key')
[ -n "$key" ] && [ "$key" != null ] || die "preauthkey"

param=/headlamp/e2e/tailscaled
state="arn:aws:ssm:eu-central-1:000000000000:parameter$param"
# tailscaled's AWS SDK reads these. The endpoint points SSM at LocalStack; IMDS is off (no instance here).
aws_env=(-e AWS_REGION=eu-central-1 -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test
  -e AWS_EC2_METADATA_DISABLED=true -e "AWS_ENDPOINT_URL_SSM=http://$ls:4566")
# client <name> <state> [authkey]: a fresh container, like a new instance from the launch template.
client() {
  local name="$1" st="$2" ak="${3:-}"
  dk run -d --name "$name" --network "$net" --entrypoint sh "${aws_env[@]}" "$ts_image" -c \
    "tailscaled --tun=userspace-networking --socket=/tmp/ts.sock --statedir=/tmp/ts --state=$st & sleep infinity" >/dev/null || die "start $name"
  wait_for 30 docker exec "$name" tailscale --socket=/tmp/ts.sock status --json || die "tailscaled in $name"
  # With a stored state there is no key and no `up`: tailscaled logs in by itself (what headlamp-logged-in checks).
  if [ -n "$ak" ]; then
    dk exec "$name" tailscale --socket=/tmp/ts.sock up --login-server="http://$hs" --hostname=cobalt \
      --accept-dns=false --auth-key="$ak" >/dev/null 2>&1 || die "up in $name"
  fi
  wait_for 30 sh -c "docker exec $name tailscale --socket=/tmp/ts.sock status --json | jq -e '.BackendState == \"Running\"'" || die "$name did not reach Running"
}
nodes() { hsc nodes list -o json; }

echo "# 1. first instance joins with the auth key, state in SSM"
client "$c1" "$state" "$key"
n1=$(nodes); id1=$(jq -r '.[0].id' <<<"$n1"); name1=$(jq -r '.[0].given_name' <<<"$n1"); ip1=$(jq -r '.[0].ip_addresses[0]' <<<"$n1")
[ "$(jq length <<<"$n1")" = 1 ] && ok "one node after the first join (id $id1, $name1, $ip1)" || nok "one node after the first join"
ptype=$(dk exec "$ls" awslocal ssm get-parameter --name "$param" --region eu-central-1 --query Parameter.Type --output text 2>/dev/null)
[ "$ptype" = SecureString ] && ok "state parameter is a SecureString" || nok "state parameter type is '$ptype', want SecureString"
dk exec "$c1" sh -c 'ls /tmp/ts/*.state /var/lib/tailscale/tailscaled.state 2>/dev/null' | grep -q . \
  && nok "a state file is on the disk" || ok "no state file on the disk"

echo "# 2. the instance goes away (scale to 0); a NEW instance starts with the same parameter and no key"
dk rm -f "$c1" >/dev/null
client "$c2" "$state"
st2=$(docker exec "$c2" tailscale --socket=/tmp/ts.sock status --json | jq -r .BackendState)
[ "$st2" = Running ] && ok "new instance is Running without an auth key" || nok "new instance state is $st2"
n2=$(nodes)
[ "$(jq length <<<"$n2")" = 1 ] && ok "still one node: no new registration" || nok "headscale has $(jq length <<<"$n2") nodes, want 1"
[ "$(jq -r '.[0].id' <<<"$n2")" = "$id1" ] && ok "same node id ($id1)" || nok "node id changed to $(jq -r '.[0].id' <<<"$n2")"
[ "$(jq -r '.[0].given_name' <<<"$n2")" = "$name1" ] && ok "same given name ($name1)" || nok "given name changed to $(jq -r '.[0].given_name' <<<"$n2")"
[ "$(jq -r '.[0].ip_addresses[0]' <<<"$n2")" = "$ip1" ] && ok "same IPv4 ($ip1)" || nok "IPv4 changed to $(jq -r '.[0].ip_addresses[0]' <<<"$n2")"
[ "$(jq -r '.[0].online' <<<"$n2")" = true ] && ok "node is online" || nok "node is not online"

echo "# 3. control: file state on the instance disk (the v0.13.0 default) with the same reusable key"
dk rm -f "$c2" >/dev/null
client "$c3" /tmp/ts/tailscaled.state "$key"
dk rm -f "$c3" >/dev/null
client "$c4" /tmp/ts/tailscaled.state "$key"
n4=$(nodes)
[ "$(jq length <<<"$n4")" -ge 3 ] && ok "file state: each new instance is a new node ($(jq -r '[.[].given_name]|join(", ")' <<<"$n4"))" \
  || nok "file state did not make new nodes; the control case is wrong"

echo "# $pass passed, $fails failed"
[ "$fails" = 0 ]

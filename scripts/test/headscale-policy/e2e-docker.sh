#!/bin/sh
# On-demand end-to-end test of `headscale-policy.sh bootstrap` against a real headscale container. Not part of the unit
# tests (make test-headscale-policy); run it with `make test-headscale-policy-e2e`. Needs docker, curl and jq.
#   E2E_HEADSCALE_IMAGE  image to run, default jauderho/headscale:v0.29.3
set -u
here=$(cd "$(dirname "$0")" && pwd)
cli="$here/../../headscale-policy.sh"
image=${E2E_HEADSCALE_IMAGE:-jauderho/headscale:v0.29.3}

skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
command -v jq >/dev/null 2>&1 || skip "jq is not installed"
command -v curl >/dev/null 2>&1 || skip "curl is not installed"

# bounded: every docker call is capped (perl is used as a portable timeout)
tmo() { if command -v perl >/dev/null 2>&1; then perl -e 'alarm shift; exec @ARGV' "$@"; else shift; "$@"; fi; }
dk() { tmo 60 docker "$@"; }
dk image inspect "$image" >/dev/null 2>&1 || tmo 300 docker pull -q "$image" >/dev/null 2>&1 || skip "cannot get image $image"

T=$(mktemp -d)
suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
ctr="hs-policy-e2e-$suffix"; vol="hs-policy-e2e-$suffix"
cleanup() { dk rm -f "$ctr" >/dev/null 2>&1; dk volume rm -f "$vol" >/dev/null 2>&1; rm -rf "$T"; }
trap cleanup EXIT
trap 'exit 130' INT TERM

pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }
check() { if [ "$2" = 0 ]; then ok "$1"; else nok "$1"; fi; }

mkdir -p "$T/etc" "$T/bin"
cat > "$T/etc/config.yaml" <<'YAML'
server_url: http://127.0.0.1:18080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: true
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: false
  urls:
    - https://controlplane.tailscale.com/derpmap/default
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /var/lib/headscale/db.sqlite
dns:
  magic_dns: true
  base_domain: scratch.example.test
  nameservers:
    global:
      - 1.1.1.1
policy:
  mode: file
  path: /etc/headscale/acl.json
unix_socket: /var/run/headscale/headscale.sock
log:
  level: info
YAML
# the same config with the whole policy block replaced (what the server runs after the switch)
awk '/^policy:/ { print; print "  mode: database"; skip = 1; next } skip && /^[[:space:]]/ { next } { skip = 0; print }' \
  "$T/etc/config.yaml" > "$T/etc/config-db.yaml"
printf '%s\n' '{"acls":[{"action":"accept","src":["*"],"dst":["*:80"]}]}' > "$T/etc/acl.json"

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])' 2>/dev/null || echo $((20000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 20000)))

start() { # config file name in /etc/headscale
  dk run -d --name "$ctr" --entrypoint "" -p "127.0.0.1:$port:8080" -v "$vol:/var/lib/headscale" \
    -v "$T/etc:/etc/headscale:ro" "$image" sh -c "mkdir -p /var/run/headscale && exec headscale serve -c /etc/headscale/$1" >/dev/null 2>&1 || return 1
  i=0
  while [ "$i" -lt 40 ]; do
    tmo 5 curl -fs "http://127.0.0.1:$port/health" >/dev/null 2>&1 && return 0
    i=$((i + 1)); sleep 1
  done
  return 1
}
hs() { dk exec "$ctr" headscale "$@"; }
dbget() { hs policy get --bypass-grpc-and-access-database-directly --force -c /etc/headscale/config-db.yaml; }
sameJSON() { # expected-json-text actual-file
  printf '%s' "$1" | jq -S . > "$T/exp.n" 2>/dev/null && jq -S . "$2" > "$T/act.n" 2>/dev/null && cmp -s "$T/exp.n" "$T/act.n"
}

# the flyctl shim: `flyctl ssh console -a APP -C CMD` -> docker exec in the scratch container
cat > "$T/bin/flyctl" <<'SHIM'
#!/bin/sh
cmd=
while [ $# -gt 0 ]; do case $1 in -C) cmd=$2; shift ;; esac; shift; done
exec docker exec "$E2E_CTR" sh -c "$cmd"
SHIM
chmod +x "$T/bin/flyctl"

dk volume create "$vol" >/dev/null 2>&1
start config.yaml && ok "headscale (file mode) is up" || { nok "headscale (file mode) did not start: $(dk logs "$ctr" 2>&1 | tail -5)"; echo "$pass passed, $fails failed"; exit 1; }

P1='{"acls":[{"action":"accept","src":["*"],"dst":["*:22"]}]}'
printf '%s' "$P1" > "$T/p1.json"
run_cli() { E2E_CTR=$ctr PATH="$T/bin:$PATH" HS_APP=x NODE_CONTROL_URL= sh "$cli" "$@" > "$T/out" 2> "$T/err"; rc=$?; }

# (1) plain JSON
run_cli bootstrap --file "$T/p1.json" --yes
check "bootstrap of a plain JSON policy exits 0 (rc=$rc)" "$([ $rc = 0 ]; echo $?)"
dbget > "$T/got1" 2>/dev/null; sameJSON "$P1" "$T/got1"; check "the database holds the new policy" $?
left=$(dk exec "$ctr" sh -c 'ls /tmp/policy-bootstrap.json /tmp/hs-bootstrap-config.yaml /tmp/hs-bootstrap.sh 2>/dev/null | wc -l' | tr -d ' ')
[ "$left" = 0 ]; check "the temp files are gone" $?
hs policy get -c /etc/headscale/config.yaml > "$T/live" 2>/dev/null; sameJSON "$(cat "$T/etc/acl.json")" "$T/live"
check "the running file-mode server still serves the file ACL" $?

# (2) HuJSON with a comment and a trailing comma
printf '%s\n' '{ // note' '  "acls": [ {"action":"accept","src":["*"],"dst":["*:443"],},' '  ],' '}' > "$T/p2.json"
run_cli bootstrap --file "$T/p2.json" --yes
check "bootstrap of a HuJSON policy exits 0 (rc=$rc)" "$([ $rc = 0 ]; echo $?)"
grep -q 'warning: the policy file contains comments or trailing commas' "$T/err"; check "bootstrap warns about the dropped comments" $?
P2='{"acls":[{"action":"accept","src":["*"],"dst":["*:443"]}]}'
dbget 2>/dev/null | tr '\n' ' ' | sed -E 's/,[[:space:]]*([]}])/\1/g' > "$T/got2"; sameJSON "$P2" "$T/got2"; check "the database holds the HuJSON policy (comment blanked)" $?

# (3) refusals
head -c 70000 /dev/zero | tr '\0' 'a' > "$T/big.json"
run_cli bootstrap --file "$T/big.json" --yes
[ $rc = 2 ]; check "an oversize file is refused with exit 2 (rc=$rc)" $?
run_cli bootstrap --file "$T/missing.json" --yes
[ $rc = 2 ]; check "a missing file is refused with exit 2 (rc=$rc)" $?
run_cli bootstrap --file "$T/p1.json"
[ $rc = 2 ]; check "bootstrap without --yes is refused with exit 2 (rc=$rc)" $?

# (4) switch the server to database mode on the same volume: no empty-policy window
dk rm -f "$ctr" >/dev/null 2>&1
start config-db.yaml && ok "headscale (database mode) is up" || nok "headscale (database mode) did not start: $(dk logs "$ctr" 2>&1 | tail -5)"
hs policy get -c /etc/headscale/config-db.yaml 2>/dev/null | tr '\n' ' ' | sed -E 's/,[[:space:]]*([]}])/\1/g' > "$T/live2"; sameJSON "$P2" "$T/live2"
check "after the switch the server serves the bootstrapped policy" $?

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

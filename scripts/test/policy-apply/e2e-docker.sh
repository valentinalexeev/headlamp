#!/bin/sh
# On-demand e2e test of examples/compose/control/scripts/policy-apply.sh (#261) with a real headscale in compose.
#   sh scripts/test/policy-apply/e2e-docker.sh      (make test-policy-apply-e2e)
#   E2E_HEADSCALE_IMAGE  default ghcr.io/juanfont/headscale:v0.29.7
set -u
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
image=${E2E_HEADSCALE_IMAGE:-ghcr.io/juanfont/headscale:v0.29.7}
skip() { echo "SKIP: $*"; exit 0; }
command -v docker >/dev/null 2>&1 || skip "docker is not available"
docker compose version >/dev/null 2>&1 || skip "docker compose is not available"
command -v python3 >/dev/null 2>&1 || skip "python3 is not installed"
docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null 2>&1 || skip "cannot get $image"

T=$(mktemp -d)
proj="pa-e2e-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
cleanup() { (cd "$T" && docker compose -p "$proj" down -v >/dev/null 2>&1); rm -rf "$T"; }
trap cleanup EXIT
trap 'exit 130' INT TERM
pass=0; fails=0
ok()  { echo "ok - $1"; pass=$((pass + 1)); }
nok() { echo "not ok - $1"; fails=$((fails + 1)); }

# The same layout as examples/compose/control: the policy is a read-only bind mount of config/acl.json.
mkdir -p "$T/config" "$T/scripts"
cp "$root/examples/compose/control/scripts/policy-apply.sh" "$T/scripts/"
cat > "$T/config/headscale.yaml" <<'YAML'
server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise: {private_key_path: /data/noise.key}
prefixes: {v4: 100.64.0.0/10}
derp:
  server: {enabled: true, region_id: 999, region_code: t, region_name: t, stun_listen_addr: "0.0.0.0:3478",
           private_key_path: /data/derp.key, automatically_add_embedded_derp_region: true}
  urls: []
  auto_update_enabled: false
database: {type: sqlite, sqlite: {path: /data/db.sqlite}}
dns: {magic_dns: false, override_local_dns: false}
policy: {mode: file, path: /config/acl.json}
unix_socket: /data/hs.sock
YAML
cat > "$T/compose.yaml" <<YAML
services:
  headscale:
    image: $image
    command: ["serve"]
    environment: {HEADSCALE_CONFIG: /config/headscale.yaml}
    volumes:
      - ./config/headscale.yaml:/config/headscale.yaml:ro
      - ./config/acl.json:/config/acl.json:ro
      - data:/data
volumes:
  data: {}
YAML
pol() { # tagOwners for tag:a, and one rule; HuJSON with a comment and a tests block, as the example policy has
  cat > "$T/config/acl.json" <<JSON
{
  // owners
  "tagOwners": {"tag:a": ["$1"]},
  "acls": [{"action": "accept", "src": ["$1"], "dst": ["$2"]}],
  "tests": [{"src": "tag:a", "accept": ["tag:a:22"]}]
}
JSON
}
dc() { (cd "$T" && docker compose -p "$proj" "$@"); }
apply() { (cd "$T" && COMPOSE="docker compose -p $proj" sh scripts/policy-apply.sh); }
# In file mode `policy get` reads the file, not the loaded policy. The loaded policy shows in what headscale enforces:
# ask it with a check of a test that only the loaded rules pass (headscale policy check uses the live state).
reloads() { dc logs --no-log-prefix headscale 2>&1 | grep -c "Policy reload completed"; }

pol "anna@" "*:22"
dc up -d >/dev/null 2>&1 || { echo "FATAL: compose up"; exit 1; }
i=0; while [ $i -lt 40 ]; do dc exec -T headscale headscale users list >/dev/null 2>&1 && break; sleep 1; i=$((i + 1)); done
dc exec -T headscale headscale users create anna --email anna@example.test >/dev/null || { echo "FATAL: user"; exit 1; }

echo "# up -d alone does not reload a changed acl.json (the trap)"
pol "anna@" "*:443"
dc up -d >/dev/null 2>&1; sleep 3
if [ "$(reloads)" = 0 ] && [ -z "$(dc ps -q --status exited)" ]; then ok "after up -d headscale did not reload the policy"
else nok "compose reloaded or restarted headscale by itself (the test does not show the trap)"; fi

echo "# policy-apply.sh checks, reloads and confirms"
# The tests block of this policy names tag:a, which has no node here: headscale refuses the SIGHUP reload for that,
# so the script must restart it (it only warns about failed tests at start).
if apply >"$T/out" 2>&1 && grep -q 'ok: headscale runs' "$T/out" && grep -q 'restarting headscale' "$T/out"; then
  ok "the new policy is checked and loaded (restart after a tests-only refusal)"
else nok "apply: $(tail -3 "$T/out")"; fi
# Write in place (same inode), as the bind mount needs for a plain reload.
pol "anna@" "*:8443"
python3 - "$T/config/acl.json" <<'PY'
import sys
p = sys.argv[1]
s = "".join(l for l in open(p) if '"tests"' not in l)
with open(p, "r+") as f:
    f.seek(0); f.write(s); f.truncate()
PY
n=$(reloads)
if apply >"$T/out" 2>&1 && grep -q 'ok: headscale runs' "$T/out" && [ "$(reloads)" -gt "$n" ] && ! grep -q restarting "$T/out"; then
  ok "a policy without failing tests loads with SIGHUP, no restart"
else nok "sighup: $(tail -3 "$T/out")"; fi

echo "# a new file (new inode, as git pull or sed -i writes it)"
# On Linux a single-file bind mount keeps the old inode, so the container sees the old file until it is recreated.
# Docker Desktop shares files by path, so there it sees the new one. The script must end with the new file live
# in both cases.
sed 's/8443/9443/' "$T/config/acl.json" > "$T/config/acl.json.new" && mv "$T/config/acl.json.new" "$T/config/acl.json"
if dc exec -T headscale headscale policy get 2>/dev/null | grep -q '9443'; then
  echo "note: this Docker shares the file by path; the recreate path is not exercised here"
else echo "note: the container sees the old file (inode pinned), as on Linux"; fi
if apply >"$T/out" 2>&1 && grep -q 'ok: headscale runs' "$T/out" && dc exec -T headscale headscale policy get 2>/dev/null | grep -q '9443'; then
  ok "after a new file, headscale runs it ($(grep -o 'recreate' "$T/out" | head -1 || true))"
else nok "new file: $(tail -3 "$T/out")"; fi

echo "# a policy that the live users make ambiguous is refused, and the old one stays (#253)"
# A second user with anna's email: "anna@example.test" now matches two users.
dc exec -T headscale headscale users create anna2 --email anna@example.test >/dev/null
pol "anna@example.test" "*:8080"
if apply >"$T/out" 2>&1; then nok "the ambiguous policy was applied"
elif grep -q 'refuses this policy' "$T/out" && ! grep -q '== 2' "$T/out"; then ok "refused before the reload; headscale keeps the old policy"
else nok "unexpected: $(tail -3 "$T/out")"; fi

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

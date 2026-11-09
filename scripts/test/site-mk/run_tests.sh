#!/usr/bin/env bash
# Tests for mk/site.mk and scripts/deploy-fly.sh (#194). It makes a throwaway site repo with this checkout as the
# submodule vendor/headlamp, and a fake flyctl that records its directory and args. No Fly, no network.
#   bash scripts/test/site-mk/run_tests.sh
set -uo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
pass=0; fails=0
ok()  { echo "ok   $1"; pass=$((pass + 1)); }
nok() { echo "FAIL $1"; fails=$((fails + 1)); }
export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

# A copy of this checkout as a plain repo with one commit (the working tree, so uncommitted changes are tested too).
core="$T/core"; mkdir -p "$core"
(cd "$root" && git ls-files -co --exclude-standard -z | while IFS= read -r -d '' f; do [ -f "$f" ] && printf '%s\0' "$f"; done \
  | tar --null -T - -cf -) | tar -xf - -C "$core"
[ -d "$core/apps" ] && [ -f "$core/mk/site.mk" ] || { echo "FATAL: the copy of the checkout is incomplete"; exit 1; }
git -C "$core" init -q && git -C "$core" add -A && git -C "$core" commit -qm core

site="$T/site"; mkdir -p "$site" && cd "$site" || exit 1; site="$(pwd -P)"
git init -q
git -c protocol.file.allow=always submodule -q add "$core" vendor/headlamp
mkdir -p fly/exit-node fly/node-control fly/headscale ansible bin
echo "direct.example" > fly/exit-node/direct-hosts.txt
echo "{}" > fly/node-control/nodes.json
echo 'app = "x"' > fly/headscale/fly.toml
echo "1.104.9" > TAILSCALE_VERSION
cat > Makefile <<'EOF'
H := vendor/headlamp
FLY_APPS := exit-node node-control
include $(H)/mk/site.mk
EOF
cat > bin/flyctl <<'EOF'
#!/bin/sh
{ echo "pwd=$(pwd)"; echo "args=$*"; ls; } > "$FLY_LOG"
EOF
chmod +x bin/flyctl
export FLYCTL="$site/bin/flyctl" FLY_LOG="$T/fly.log"
git add -A && git commit -qm site

hlv="$(tr -d '[:space:]' < vendor/headlamp/VERSION)"

# 1. deploy with build args from the pins, from a subdirectory, and the submodule is clean after it
(cd ansible && make -s -C .. deploy-exit-node >/dev/null 2>&1); rc=$?
[ $rc = 0 ] && ok "deploy-exit-node runs (rc 0)" || nok "deploy-exit-node rc $rc"
grep -q "args=deploy --build-arg TSVERSION=1.104.9" "$FLY_LOG" && ok "exit-node gets TSVERSION from the site pin" || nok "TSVERSION: $(grep args "$FLY_LOG")"
grep -q "VERSION=$hlv" "$FLY_LOG" && nok "exit-node got VERSION, but its Dockerfile has no ARG VERSION" || ok "exit-node gets no VERSION (no ARG)"
grep -q "pwd=$(cd "$site" && pwd -P)/vendor/headlamp/apps/exit-node" "$FLY_LOG" && ok "flyctl runs in the vendored app dir" || nok "pwd: $(grep pwd "$FLY_LOG")"
grep -qx "direct-hosts.txt" "$FLY_LOG" && ok "the overlay file is there during the deploy" || nok "overlay missing"
[ -z "$(git -C vendor/headlamp status --porcelain)" ] && ok "submodule is clean after the deploy" || nok "submodule dirty after deploy"

(cd ansible && ../vendor/headlamp/scripts/deploy-fly.sh node-control >/dev/null 2>&1) && grep -q "VERSION=$hlv" "$FLY_LOG" \
  && grep -q "TSVERSION=1.104.9" "$FLY_LOG" && ok "deploy-fly.sh from a subdirectory: node-control gets VERSION=$hlv and TSVERSION" \
  || nok "node-control args: $(grep args "$FLY_LOG")"

# 2. the caller's build arg wins
make -s deploy-exit-node ARGS="--build-arg TSVERSION=9.9.9" >/dev/null 2>&1
grep -q "args=deploy --build-arg TSVERSION=9.9.9$" "$FLY_LOG" && ok "a --build-arg from the caller wins (no second TSVERSION)" || nok "override: $(grep args "$FLY_LOG")"

# 3. dry run deploys nothing
rm -f "$FLY_LOG"
out="$(make -s plan-deploy-node-control 2>&1)"
[ ! -e "$FLY_LOG" ] && printf '%s' "$out" | grep -q "would run in vendor/headlamp/apps/node-control" && ok "plan-deploy-<app> prints and does not deploy" || nok "dry run: $out"

# 4. refusals
out="$(make -s deploy-headscale-nope 2>&1)"; [ $? != 0 ] && ok "unknown app is refused" || nok "unknown app: $out"
rm -rf fly/headscale && out="$(vendor/headlamp/scripts/deploy-fly.sh headscale 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "no overlay" \
  && ok "no overlay is refused" || nok "no overlay: $out"; git checkout -q -- fly
touch vendor/headlamp/stray && out="$(make -s deploy-exit-node 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "has changes" \
  && ok "a dirty submodule is refused" || nok "dirty: $out"; rm -f vendor/headlamp/stray

# 5. the pin guard: the submodule at another commit than the index
git -C vendor/headlamp commit -q --allow-empty -m bump
out="$(make -s submodule-check 2>&1)"; rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q "but the index pins" && ok "submodule-check refuses a pin and checkout that differ" || nok "submodule-check: $out"
bumped="$(git -C vendor/headlamp rev-parse HEAD)"
make -s submodule >/dev/null 2>&1
[ "$(git -C vendor/headlamp rev-parse HEAD)" = "$bumped" ] && ok "make submodule does not reset an uncommitted pin bump" || nok "make submodule reset the bump"
out="$(make -s deploy-exit-node 2>&1)"; [ $? != 0 ] && ok "deploy refuses while the pin differs" || nok "deploy with a differing pin: $out"
git add vendor/headlamp
make -s submodule-check >/dev/null 2>&1 && ok "after 'git add vendor/headlamp' the check passes" || nok "check after git add"
git -C vendor/headlamp reset -q --hard HEAD~1 && git add vendor/headlamp

# 6. targets that need a value say so
out="$(make -s policy-diff 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "NODE_CONTROL_URL is not set" \
  && ok "policy-diff without NODE_CONTROL_URL stops with a clear message" || nok "policy-diff: $out"
out="$(make -s bump-tailscale-version 2>&1)"; [ $? != 0 ] && printf '%s' "$out" | grep -q "usage:" && ok "bump-tailscale-version needs VERSION" || nok "bump: $out"

# 7. verify is read-only and plans every Fly app
rm -f "$FLY_LOG"
out="$(make -s verify 2>&1)"; rc=$?
[ $rc = 0 ] && [ ! -e "$FLY_LOG" ] && printf '%s' "$out" | grep -q "tailscale pin: 1.104.9" \
  && [ "$(printf '%s' "$out" | grep -c "would run in")" = 2 ] && ok "verify: pin, two planned deploys, nothing deployed" || nok "verify rc=$rc: $out"

# 8. the site falls back to the submodule pin when it has no TAILSCALE_VERSION
git rm -q TAILSCALE_VERSION
make -s deploy-exit-node >/dev/null 2>&1
grep -q "TSVERSION=$(tr -d '[:space:]' < vendor/headlamp/TAILSCALE_VERSION)" "$FLY_LOG" && ok "no site pin: TSVERSION from the submodule pin" || nok "fallback: $(grep args "$FLY_LOG")"

# 9. the shipped example site repo: its Makefile has no recipe of its own, and it works with only its variables
ex="$core/examples/private-repo"
! grep -qE '^	' "$ex/Makefile" && ok "examples/private-repo/Makefile has no recipes (only values + include)" || nok "the example Makefile still has recipes"
sed 's|^export SOPS_AGE_KEY.*|export SOPS_AGE_KEY :=|' "$ex/Makefile" > Makefile
out="$(make -s help 2>&1)"; printf '%s' "$out" | grep -q "deploy-<app>" && ok "example Makefile: make help lists the targets" || nok "example help: $out"
mkdir -p fly/headscale && echo 'app = "x"' > fly/headscale/fly.toml
out="$(make -s plan-deploy-headscale 2>&1)"; printf '%s' "$out" | grep -q "would run in vendor/headlamp/apps/headscale" \
  && ok "example Makefile: plan-deploy-headscale" || nok "example plan: $out"
cp "$ex/fly/deploy.sh" fly/deploy.sh && chmod +x fly/deploy.sh
out="$(cd ansible && ../fly/deploy.sh --dry-run headscale 2>&1)"; printf '%s' "$out" | grep -q "would run in" \
  && ok "the deprecated fly/deploy.sh wrapper still works, from a subdirectory" || nok "wrapper: $out"

echo "$pass passed, $fails failed"
[ "$fails" = 0 ]

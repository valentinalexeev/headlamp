#!/usr/bin/env bash
# Check and rotate the headscale API key that the route approver (and optionally other apps) use. The key value is
# never printed and never passed on a command line.
#
#   rotate-headscale-api-key.sh --status      list the server's keys with days left (read-only)
#   rotate-headscale-api-key.sh --check       like --status, exit 1 when the longest-lived valid key has < WARN_DAYS left
#   rotate-headscale-api-key.sh --dry-run     show the steps
#   rotate-headscale-api-key.sh               create a key, optionally store it, verify it, stage it on the Fly apps
#   rotate-headscale-api-key.sh --deploy      ... then run DEPLOY_CMD so the apps pick it up
#   rotate-headscale-api-key.sh --expire-old  expire every key except the newest valid one (run after a deploy)
#
# Configuration (environment):
#   HS_APP         Fly app running headscale (required)
#   HS_URL         headscale base URL, to verify the new key (required to rotate)
#   STAGE_APPS     space-separated Fly apps that get HEADSCALE_API_KEY staged (required to rotate)
#   DEPLOY_CMD     shell command that redeploys those apps (used by --deploy; otherwise it is only printed)
#   HS_MACHINE     machine id for the headscale CLI; default: the started machine of process group PROCESS_GROUP
#   PROCESS_GROUP  default "app" (the group that runs `headscale serve`; a standby approver machine has no CLI socket)
#   KEY_LIFETIME   default 365d;  WARN_DAYS  default 30
#   SECRET_FILE / SECRET_FIELD   optional sops file and top-level field that also receive the new key (needs sops and
#                  SOPS_AGE_KEY); --expire-old then keeps the key stored there. Without it the newest key is kept.
set -euo pipefail

mode="${1:-rotate}"
: "${HS_APP:?HS_APP is required (the Fly app running headscale)}"
life="${KEY_LIFETIME:-365d}"; group="${PROCESS_GROUP:-app}"
stage_apps="${STAGE_APPS:-}"; hs_url="${HS_URL:-}"

machine() {
  if [ -n "${HS_MACHINE:-}" ]; then echo "$HS_MACHINE"; return; fi
  flyctl machines list -a "$HS_APP" --json | python3 -c '
import json, sys
g = sys.argv[1]
m = [x for x in json.load(sys.stdin) if x.get("state") == "started" and x.get("config", {}).get("metadata", {}).get("fly_process_group") == g]
if not m: sys.exit("no started machine in process group %r" % g)
print(m[0]["id"])' "$group"
}
hs() { flyctl ssh console -a "$HS_APP" --machine "$(machine)" -C "headscale $*" </dev/null; }

# prints "<prefix> days_left=N ok|expired"; exits 1 under the warn threshold (arg 1, days) when no valid key is long-lived
list_keys() { hs apikeys list -o json | python3 -c '
import datetime, json, sys, time
raw = sys.stdin.read(); raw = raw[raw.index("["):] if "[" in raw else "[]"
warn = float(sys.argv[1]); best = -1
for k in json.loads(raw):
    e = k.get("expiration")
    if isinstance(e, dict): e = e.get("seconds")
    elif isinstance(e, str): e = datetime.datetime.fromisoformat(e.replace("Z", "+00:00")).timestamp()
    secs = None if e is None else float(e) - time.time()
    left = None if secs is None else int(secs // 86400)
    print(k.get("prefix", "?"), "days_left=%s" % left, "expired" if secs is not None and secs <= 0 else "ok")
    if secs is None or secs > 0: best = max(best, 10**6 if secs is None else secs / 86400)
if best < warn:
    print("WARNING: the longest-lived headscale API key has %d days left (< %d): rotate it" % (best, warn), file=sys.stderr)
    sys.exit(1)' "${1:-0}"; }

# Key formats: hskey-api-<prefix>-<secret> (the list shows "hskey-api-<prefix>-***") and the legacy <prefix>.<secret>.
norm_prefix() { sed -E 's/^(hskey-api-[^-]+)-.*/\1/; s/^([^.]+)\..*/\1/; s/\*+$//'; }

case "$mode" in
  --status) list_keys; exit 0 ;;
  --check) list_keys "${WARN_DAYS:-30}"; exit $? ;;
  --dry-run)
    echo "1. $HS_APP: headscale apikeys create --expiration $life   (key captured, not printed)"
    [ -n "${SECRET_FILE:-}" ] && echo "2. store it in $SECRET_FILE as ${SECRET_FIELD:-?} (sops decrypt, edit, re-encrypt)"
    echo "3. verify: GET ${hs_url:-<HS_URL>}/api/v1/node with the new key must return 200"
    echo "4. flyctl secrets import --stage HEADSCALE_API_KEY on: ${stage_apps:-<STAGE_APPS>}"
    echo "5. (--deploy) ${DEPLOY_CMD:-<DEPLOY_CMD>}"
    echo "6. (--expire-old) expire every key except the newest valid one"
    exit 0 ;;
  --expire-old)
    if [ -n "${SECRET_FILE:-}" ]; then
      keep="$(sops decrypt --extract "[\"${SECRET_FIELD:?SECRET_FIELD is required with SECRET_FILE}\"]" "$SECRET_FILE" | norm_prefix)"
    else
      keep="$(hs apikeys list -o json | python3 -c '
import json, sys, datetime
raw = sys.stdin.read(); ks = json.loads(raw[raw.index("["):])
def exp(k):
    e = k.get("expiration"); e = e.get("seconds") if isinstance(e, dict) else e
    return datetime.datetime.fromisoformat(e.replace("Z", "+00:00")).timestamp() if isinstance(e, str) else float(e or 0)
print(max(ks, key=exp)["prefix"])' | norm_prefix)"
    fi
    [ -n "$keep" ] || { echo "cannot determine the key to keep" >&2; exit 1; }
    hs apikeys list -o json | python3 -c '
import json, sys
raw = sys.stdin.read(); print("\n".join(k["prefix"] for k in json.loads(raw[raw.index("["):])))' | norm_prefix | while read -r p; do
      [ "$p" = "$keep" ] && continue
      echo "expiring $p"; hs apikeys expire --prefix "$p" || true
    done
    list_keys; exit 0 ;;
  rotate|--deploy) ;;
  *) echo "usage: $0 [--status|--check|--dry-run|--deploy|--expire-old]" >&2; exit 64 ;;
esac

: "${HS_URL:?HS_URL is required to verify the new key}"
: "${STAGE_APPS:?STAGE_APPS is required (apps that receive HEADSCALE_API_KEY)}"

echo "== creating a key ($life)"
key="$(hs apikeys create --expiration "$life" | tr -d '\r' | tail -1)"
case "$key" in hskey-api-*-*|[A-Za-z0-9]*.[A-Za-z0-9_-]*) ;; *) echo "unexpected output from apikeys create" >&2; exit 1 ;; esac

if [ -n "${SECRET_FILE:-}" ]; then
  echo "== storing it in $SECRET_FILE"
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT; chmod 700 "$tmp"
  sops decrypt --output-type json "$SECRET_FILE" > "$tmp/s.json"
  KEY="$key" FIELD="${SECRET_FIELD:?SECRET_FIELD is required with SECRET_FILE}" P="$tmp/s.json" python3 -c '
import json, os
d = json.load(open(os.environ["P"])); d[os.environ["FIELD"]] = os.environ["KEY"]; json.dump(d, open(os.environ["P"], "w"))'
  sops encrypt --input-type json --output-type yaml --filename-override "$SECRET_FILE" "$tmp/s.json" > "$tmp/s.yaml"
  sops decrypt "$tmp/s.yaml" >/dev/null && cp "$tmp/s.yaml" "$SECRET_FILE"
  rm -f "$tmp/s.json"
fi

echo "== verifying the new key against the API"
code="$(printf 'header = "Authorization: Bearer %s"\n' "$key" | curl -s -o /dev/null -w '%{http_code}' -K - "${hs_url%/}/api/v1/node")"
[ "$code" = 200 ] || { echo "new key rejected (HTTP $code); nothing staged" >&2; exit 1; }

echo "== staging on: $STAGE_APPS"
for app in $STAGE_APPS; do
  printf 'HEADSCALE_API_KEY=%s\n' "$key" | flyctl secrets import -a "$app" --stage
done
unset key

if [ "$mode" = "--deploy" ] && [ -n "${DEPLOY_CMD:-}" ]; then
  echo "== deploying: $DEPLOY_CMD"
  sh -c "$DEPLOY_CMD"
  echo "deployed. Check the approver logs for 200s, then run with --expire-old"
else
  echo "staged, not deployed. Redeploy: ${DEPLOY_CMD:-<the staged apps>}, then run with --expire-old"
fi
[ -z "${SECRET_FILE:-}" ] || echo "commit $SECRET_FILE (it changed)"

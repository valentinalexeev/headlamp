#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 [--dry-run] <preauth-key-id> <hostname> [ssh-target]" >&2
  echo "  If ssh-target is omitted, prints the tailscale up command for the operator" >&2
  echo "  to run by hand on the target device instead of running it remotely." >&2
  exit 1
}

# Required: HEADSCALE_APP (Fly app name) and HEADSCALE_LOGIN_SERVER (https://...).
: "${HEADSCALE_APP:?set HEADSCALE_APP}"
: "${HEADSCALE_LOGIN_SERVER:?set HEADSCALE_LOGIN_SERVER}"
DRY_RUN=false
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
  shift
fi

[[ $# -ge 2 ]] || usage
KEY_ID="$1"
HOSTNAME="$2"
SSH_TARGET="${3:-}"

run() {
  if $DRY_RUN; then
    echo "[dry-run] $*"
  else
    echo "+ $*"
    eval "$@"
  fi
}

# Like run(), but takes a separate DISPLAY string to echo instead of the
# real command — used when the real command embeds the resolved authkey,
# so it's never printed to stdout/logs/scrollback.
run_redacted() {
  local display="$1"
  shift
  if $DRY_RUN; then
    echo "[dry-run] $display"
  else
    echo "+ $display"
    eval "$@"
  fi
}

echo "== Step 1: resolve the key id to its actual key material =="
list_cmd="flyctl ssh console -a ${HEADSCALE_APP} -C \"headscale preauthkeys list --output json\""
if $DRY_RUN; then
  echo "[dry-run] $list_cmd"
  echo "[dry-run] (would parse the JSON for id == $KEY_ID and extract .key)"
  RESOLVED_KEY="DRY-RUN-PLACEHOLDER-KEY"
else
  RESOLVED_KEY="$(eval "$list_cmd" | jq -r --arg id "$KEY_ID" '.[] | select(.id == $id) | .key')"
  if [[ -z "$RESOLVED_KEY" || "$RESOLVED_KEY" == "null" ]]; then
    echo "No preauth key found with id $KEY_ID" >&2
    exit 1
  fi
fi

LOGIN_SERVER="${HEADSCALE_LOGIN_SERVER}"
UP_CMD="tailscale up --login-server=${LOGIN_SERVER} --authkey=${RESOLVED_KEY} --hostname=${HOSTNAME}"

echo "== Step 2: register the host =="
if [[ -n "$SSH_TARGET" ]]; then
  run_redacted \
    "ssh $SSH_TARGET 'tailscale up --login-server=${LOGIN_SERVER} --authkey=<redacted> --hostname=${HOSTNAME}'" \
    "ssh $SSH_TARGET '$UP_CMD'"
else
  # No ssh-target: the whole point of this branch is to hand the operator
  # a command to run themselves on a device this script can't reach, so
  # the real key is necessarily shown here — there's no remote execution
  # to redact it from.
  echo "No ssh-target given. Run this on the target device:"
  echo ""
  echo "  $UP_CMD"
  echo ""
fi

echo "== Step 3: verify registration =="
verify_cmd="flyctl ssh console -a ${HEADSCALE_APP} -C \"headscale nodes list\""
if $DRY_RUN; then
  echo "[dry-run] $verify_cmd"
  echo "[dry-run] (would confirm $HOSTNAME appears under the expected user)"
else
  eval "$verify_cmd" | grep -- "$HOSTNAME" || {
    echo "WARNING: $HOSTNAME not found in headscale nodes list — registration may still be in progress." >&2
    exit 1
  }
  echo "$HOSTNAME registered."
fi

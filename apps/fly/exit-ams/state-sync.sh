#!/bin/sh
# Keeps tailscaled's state file in Redis, encrypted with age, so the node identity survives redeploys.
#   state-sync.sh restore   before tailscaled: pull the state from Redis (or seed Redis from the local file)
#   state-sync.sh watch     after tailscaled: push the state file whenever its checksum changes (loops forever)
#   state-sync.sh push      encrypt and store the state file once
# Exit codes: 0 ok, 1 fatal, 64 bad usage.
#
# Env: REDIS_URL (secret; unset = sync off, exit 0 and do nothing), STATE_AGE_IDENTITY (secret, AGE-SECRET-KEY-...),
#      STATE_REDIS_KEY (default exit-node:tailscaled-state), STATE_FILE (default /var/lib/tailscale/tailscaled.state),
#      STATE_POLL_SECONDS (default 60), BOOT_WAIT_SECONDS (how long restore waits for Redis, default 60).
# Values are never logged.
set -eu

STATE_FILE="${STATE_FILE:-/var/lib/tailscale/tailscaled.state}"
STATE_REDIS_KEY="${STATE_REDIS_KEY:-exit-node:tailscaled-state}"
STATE_POLL_SECONDS="${STATE_POLL_SECONDS:-60}"
BOOT_WAIT_SECONDS="${BOOT_WAIT_SECONDS:-60}"
REDIS_URL="${REDIS_URL:-}"
IDFILE=""
RECIPIENT=""
WORK=""

log() { echo "state-sync: $*"; }
err() { echo "state-sync: $*" >&2; }

cleanup() {
  [ -z "$IDFILE" ] || rm -f "$IDFILE"
  [ -z "$WORK" ] || rm -f "$WORK" "$WORK.enc"
}
trap cleanup EXIT
trap 'exit 143' TERM INT

redis() { redis-cli -u "$REDIS_URL" --no-auth-warning "$@"; }

enabled() { [ -n "$REDIS_URL" ]; }

# Write the identity to a private temp file and derive the recipient once.
setup_identity() {
  if [ -z "${STATE_AGE_IDENTITY:-}" ]; then
    err "REDIS_URL is set but STATE_AGE_IDENTITY is not"
    return 1
  fi
  umask 077
  IDFILE="$(mktemp)"
  chmod 600 "$IDFILE"
  printf '%s\n' "$STATE_AGE_IDENTITY" > "$IDFILE"
  RECIPIENT="$(age-keygen -y "$IDFILE")" || { err "cannot derive age recipient from STATE_AGE_IDENTITY"; return 1; }
  [ -n "$RECIPIENT" ] || { err "empty age recipient"; return 1; }
  # Scratch file for restore/push, next to the state file so mv stays on one filesystem.
  mkdir -p "$(dirname "$STATE_FILE")"
  WORK="$(mktemp "$STATE_FILE.XXXXXX")"
}

wait_redis() {
  waited=0
  while :; do
    if redis PING >/dev/null 2>&1; then return 0; fi
    [ "$waited" -lt "$BOOT_WAIT_SECONDS" ] || return 1
    sleep 1
    waited=$((waited + 1))
  done
}

file_sum() { if [ -s "$STATE_FILE" ]; then cksum < "$STATE_FILE"; else echo none; fi; }

push() {
  if [ ! -s "$STATE_FILE" ]; then
    log "no state file to push"
    return 0
  fi
  if ! age -r "$RECIPIENT" -a < "$STATE_FILE" > "$WORK.enc"; then
    err "encryption failed"
    return 1
  fi
  if ! redis -x SET "$STATE_REDIS_KEY" < "$WORK.enc" >/dev/null; then
    err "SET failed; will retry"
    return 1
  fi
  return 0
}

restore() {
  if ! wait_redis; then
    if [ -s "$STATE_FILE" ]; then
      err "Redis unreachable after ${BOOT_WAIT_SECONDS}s; falling back to the local state file"
      return 0
    fi
    err "Redis unreachable after ${BOOT_WAIT_SECONDS}s and no local state file; refusing to start a new identity"
    return 1
  fi
  exists="$(redis EXISTS "$STATE_REDIS_KEY")" || { err "EXISTS failed"; return 1; }
  if [ "$exists" = 1 ]; then
    redis GET "$STATE_REDIS_KEY" > "$WORK.enc" || { err "GET failed"; return 1; }
    if ! age -d -i "$IDFILE" < "$WORK.enc" > "$WORK"; then
      err "decrypt failed; leaving the local state file untouched"
      return 1
    fi
    if [ ! -s "$WORK" ]; then
      err "decrypted state is empty; leaving the local state file untouched"
      return 1
    fi
    mv "$WORK" "$STATE_FILE"
    WORK=""
    log "restored state from Redis"
  elif [ -s "$STATE_FILE" ]; then
    log "no state in Redis; seeding it from the local state file"
    push
  else
    log "no state in Redis and no local file; starting fresh"
  fi
}

watch() {
  last="$(file_sum)"
  log "watching"
  while :; do
    sleep "$STATE_POLL_SECONDS"
    cur="$(file_sum)"
    if [ "$cur" != "$last" ] && [ "$cur" != none ]; then
      if push; then
        last="$cur"
        log "pushed"
      fi
    fi
  done
}

main() {
  case "${1:-}" in
    restore|watch|push) ;;
    *) err "usage: state-sync.sh restore|watch|push"; exit 64 ;;
  esac
  if ! enabled; then
    log "REDIS_URL not set; sync is off"
    exit 0
  fi
  setup_identity || exit 1
  "$1"
}

main "$@"

#!/bin/sh
# Keeps tailscaled's state file in Redis, encrypted with age, so the node identity survives redeploys.
#   state-sync.sh restore   before tailscaled: pull the state from Redis (or seed Redis from the local file)
#   state-sync.sh watch     after tailscaled: push the state file whenever its checksum changes (loops forever)
#   state-sync.sh push      encrypt and store the state file once
# Exit codes: 0 ok, 1 fatal, 64 bad usage.
#
# Env: REDIS_URL (secret; unset = sync off, exit 0 and do nothing), STATE_AGE_IDENTITY (secret, AGE-SECRET-KEY-...),
#      STATE_REDIS_KEY (default exit-node:tailscaled-state), STATE_FILE (default /var/lib/tailscale/tailscaled.state),
#      STATE_POLL_SECONDS (default 60), BOOT_WAIT_SECONDS (wall-clock seconds restore waits for Redis, default 60),
#      REDIS_TIMEOUT_SECONDS (per redis-cli call, default 5; needs a `timeout` binary).
# The password in REDIS_URL is handed to redis-cli through REDISCLI_AUTH, never on a command line. Values are never logged.
set -eu

STATE_FILE="${STATE_FILE:-/var/lib/tailscale/tailscaled.state}"
STATE_REDIS_KEY="${STATE_REDIS_KEY:-exit-node:tailscaled-state}"
STATE_POLL_SECONDS="${STATE_POLL_SECONDS:-60}"
BOOT_WAIT_SECONDS="${BOOT_WAIT_SECONDS:-60}"
REDIS_TIMEOUT_SECONDS="${REDIS_TIMEOUT_SECONDS:-5}"
REDIS_URL="${REDIS_URL:-}"
REDIS_CLI_URL=""
REDIS_USER=""
IDFILE=""
RECIPIENT=""
WORK=""
SLEEP_PID=""

log() { echo "state-sync: $*"; }
err() { echo "state-sync: $*" >&2; }

cleanup() {
  [ -z "$IDFILE" ] || rm -f "$IDFILE"
  drop_work
  [ -z "$SLEEP_PID" ] || kill "$SLEEP_PID" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 143' TERM INT

# Decode %XX escapes the way redis-cli does for the userinfo of a URL. A '%' not followed by two hex digits stays as is
# (redis-cli would reject it). A decoded newline or NUL is lost, which no real password needs.
url_decode() {
  _in="$1"
  _out=""
  while :; do
    case "$_in" in *%[0-9A-Fa-f][0-9A-Fa-f]*) ;; *) break ;; esac
    _head="${_in%%%[0-9A-Fa-f][0-9A-Fa-f]*}"
    _rest="${_in#"$_head"%}"
    _hex="${_rest%"${_rest#??}"}"
    # shellcheck disable=SC2059 # the format is an octal escape built from validated hex digits
    _char="$(printf "\\$(printf '%03o' "$((0x$_hex))")")"
    _out="$_out$_head$_char"
    _in="${_rest#??}"
  done
  printf '%s' "$_out$_in"
}

# Split REDIS_URL so the password never appears in redis-cli's arguments: it goes to REDISCLI_AUTH, the user name to
# --user, and the rest (scheme, host, port, db) stays in the -u URL. Userinfo is everything up to the last '@'; with no
# ':' in it, it is the password (as redis-cli reads it).
parse_redis_url() {
  REDIS_CLI_URL="$REDIS_URL"
  REDIS_USER=""
  case "$REDIS_URL" in
    redis://*@*|rediss://*@*) ;;
    *) return 0 ;;
  esac
  _scheme="${REDIS_URL%%://*}://"
  _rest="${REDIS_URL#*://}"
  _info="${_rest%@*}"
  REDIS_CLI_URL="$_scheme${_rest##*@}"
  case "$_info" in
    *:*) REDIS_USER="$(url_decode "${_info%%:*}")"; _pass="${_info#*:}" ;;
    *) _pass="$_info" ;;
  esac
  if [ -n "$_pass" ]; then
    REDISCLI_AUTH="$(url_decode "$_pass")"
    export REDISCLI_AUTH
  fi
}

# Every Redis call is bounded by `timeout` (busybox/coreutils) when available, so a hung connect cannot stall the
# boot wait or a shutdown push; without it the call is unbounded.
redis() {
  if command -v timeout >/dev/null 2>&1; then
    timeout "$REDIS_TIMEOUT_SECONDS" redis-cli -u "$REDIS_CLI_URL" ${REDIS_USER:+--user "$REDIS_USER"} --no-auth-warning "$@"
  else
    redis-cli -u "$REDIS_CLI_URL" ${REDIS_USER:+--user "$REDIS_USER"} --no-auth-warning "$@"
  fi
}

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
}

# Scratch files for one restore/push, next to the state file so mv stays on one filesystem. They live only for the
# operation (drop_work), not for the watcher's lifetime.
new_work() {
  mkdir -p "$(dirname "$STATE_FILE")"
  WORK="$(mktemp "$STATE_FILE.sync.XXXXXX")"
}
drop_work() {
  [ -z "$WORK" ] || rm -f "$WORK" "$WORK.enc"
  WORK=""
}

wait_redis() {
  started="$(date +%s)"
  while :; do
    pong="$(redis PING 2>/dev/null)" || pong=""
    if [ "$pong" = PONG ]; then return 0; fi
    [ $(($(date +%s) - started)) -lt "$BOOT_WAIT_SECONDS" ] || return 1
    sleep 1
  done
}

file_sum() { if [ -s "$STATE_FILE" ]; then cksum < "$STATE_FILE"; else echo none; fi; }

push() {
  if [ ! -s "$STATE_FILE" ]; then
    log "no state file to push"
    return 0
  fi
  new_work
  if ! age -r "$RECIPIENT" -a < "$STATE_FILE" > "$WORK.enc"; then
    err "encryption failed"
    drop_work
    return 1
  fi
  if ! redis -x SET "$STATE_REDIS_KEY" < "$WORK.enc" >/dev/null; then
    err "SET failed; will retry"
    drop_work
    return 1
  fi
  drop_work
  return 0
}

restore() {
  # Scratch files a killed run left behind (restore runs before the watcher, so none is in use).
  rm -f "$STATE_FILE".sync.??????  "$STATE_FILE".sync.??????.enc
  if ! wait_redis; then
    if [ -s "$STATE_FILE" ]; then
      err "Redis unreachable after ${BOOT_WAIT_SECONDS}s; falling back to the local state file"
      return 0
    fi
    err "Redis unreachable after ${BOOT_WAIT_SECONDS}s and no local state file; refusing to start a new identity"
    return 1
  fi
  exists="$(redis EXISTS "$STATE_REDIS_KEY")" || { err "EXISTS failed"; return 1; }
  case "$exists" in
    0|1) ;;
    *) err "unexpected reply to EXISTS; refusing to continue"; return 1 ;;
  esac
  if [ "$exists" = 1 ]; then
    new_work
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
    drop_work
    log "restored state from Redis"
  elif [ -s "$STATE_FILE" ]; then
    log "no state in Redis; seeding it from the local state file"
    push || err "seeding Redis failed; continuing with the local state file (the watcher retries)"
  else
    log "no state in Redis and no local file; starting fresh"
  fi
}

watch() {
  # Start from "none" so the first cycle pushes whatever the file holds by then (state written
  # during boot, or a local file that diverged during an outage). No-op if the file is empty.
  last=none
  log "watching"
  while :; do
    # Background sleep + wait, so a TERM runs the trap at once instead of after the poll interval.
    sleep "$STATE_POLL_SECONDS" &
    SLEEP_PID=$!
    wait "$SLEEP_PID" || true
    SLEEP_PID=""
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
  parse_redis_url
  setup_identity || exit 1
  "$1"
}

main "$@"

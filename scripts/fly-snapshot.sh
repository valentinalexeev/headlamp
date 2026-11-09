#!/bin/sh
# Take a restore point of a Fly app's volume before a deploy (#197): make a volume snapshot, wait until Fly reports it
# as created, and print its ID. Exit non-zero when that fails, so the deploy that follows does not run.
#
#   fly-snapshot.sh <fly-app> [volume-name]     volume-name default: the app's only volume (refuses when it has several)
#
# Fly's `volumes snapshots create` only schedules the snapshot and prints no ID. So this script lists the snapshots
# before, creates one, and polls the list until a new snapshot with status "created" is there.
#
# Environment:
#   FLYCTL              the flyctl command (default flyctl; tests set a fake)
#   SNAPSHOT_TIMEOUT    seconds to wait for the snapshot (default 600)
#   SNAPSHOT_POLL       seconds between polls (default 5)
set -eu
app="${1:?usage: fly-snapshot.sh <fly-app> [volume-name]}"
want="${2:-}"
fly="${FLYCTL:-flyctl}"
timeout="${SNAPSHOT_TIMEOUT:-600}"; poll="${SNAPSHOT_POLL:-5}"
die() { echo "fly-snapshot: $*" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || die "jq is required"

vols="$($fly volumes list -a "$app" --json)" || die "cannot list the volumes of $app"
if [ -n "$want" ]; then
  vid="$(printf '%s' "$vols" | jq -r --arg n "$want" '[.[] | select(.name == $n)] | if length == 1 then .[0].id else "" end')"
  [ -n "$vid" ] || die "$app has no single volume named $want"
else
  n="$(printf '%s' "$vols" | jq 'length')"
  [ "$n" = 1 ] || die "$app has $n volumes; give the volume name"
  vid="$(printf '%s' "$vols" | jq -r '.[0].id')"
fi

before="$($fly volumes snapshots list "$vid" --json | jq -r '[.[].id] | join(" ")')" || die "cannot list the snapshots of $vid"
$fly volumes snapshots create "$vid" -a "$app" >/dev/null || die "cannot create a snapshot of $vid"
echo "fly-snapshot: snapshot of $app volume $vid scheduled; waiting (up to ${timeout}s)" >&2

deadline=$(( $(date +%s) + timeout ))
while :; do
  new="$($fly volumes snapshots list "$vid" --json | jq -r --arg before " $before " '
    [.[] | .id as $id | select(($before | contains(" " + $id + " ")) | not)] | sort_by(.created_at) | last // empty
    | "\(.id) \(.status)"')" || new=""
  case "$new" in
    *" created") id="${new% *}"; echo "fly-snapshot: $app volume $vid: snapshot $id is ready" >&2; echo "$id"; exit 0 ;;
    *" failed" | *" error"*) die "snapshot ${new% *} of $vid failed" ;;
  esac
  [ "$(date +%s)" -lt "$deadline" ] || break
  sleep "$poll"
done
die "no ready snapshot of $vid after ${timeout}s"

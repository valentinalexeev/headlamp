#!/bin/sh
# Back up and restore the enrolment data of a node-control app that keeps it in Redis (ENROLL_REDIS=1), through
# `fly ssh console`. The data is never written to disk unencrypted: it goes from the app's `enroll-export` straight into
# the encryption tool.
#
# usage:
#   enroll-backup.sh backup  <fly-app> <out-file> (--sops | --age <recipient> | --plaintext)
#   enroll-backup.sh restore <fly-app> <in-file>  (--sops | --age-identity <file> | --plaintext) [--force]
#
#   --sops              encrypt with `sops` (output format picked from <out-file>, e.g. backup.sops.json, so your
#                       .sops.yaml creation rules apply); restore decrypts with `sops -d`
#   --age <recipient>   encrypt to an age recipient (public key); restore needs --age-identity <file>
#   --plaintext         NO encryption (the file holds device names, owners and credentials; written mode 0600).
#                       Only for a throwaway file; refused unless given explicitly.
#   --force             restore into a Redis that already holds data (overwrites records with the same ids, never
#                       deletes others); without it a populated store refuses
#
# backup: runs `node-control enroll-export` in the machine, keeps the output in memory, checks that it is the expected JSON
# (devices array, approvals array or null (an app with no approvals exports null), numeric counter, at least one device) BEFORE encrypting, writes <out-file> atomically (an
# encrypted temp file in the same directory, then rename; an existing file is never overwritten), verifies that no device
# UUID appears in the encrypted result, and prints counts only (never names, owners or UUIDs). One-time links are not part
# of an export. With --sops the key NAMES of the JSON stay readable (sops encrypts values only).
# restore: decrypts <in-file> and pipes it into `node-control enroll-import -json -` in the machine (stdin through
# `fly ssh console -C`); the app checks the shape, writes, and verifies the copy by reading it back.
#
# Needs flyctl logged in, jq, and sops or age for the chosen mode. The app must already run with Redis enrolment.
set -u

usage() {
  sed -n '2,/^set -u/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}
die() { echo "$0: $*" >&2; exit 2; }

[ "$#" -ge 3 ] || usage
mode=$1; app=$2; file=$3; shift 3
case $mode in backup | restore) ;; *) usage ;; esac
case $app in -* | '') die "app name must not start with - or be empty" ;; esac
case $file in -* | '') die "file must not start with - or be empty" ;; esac

enc=
recipient=
identity=
force=0
while [ "$#" -gt 0 ]; do
  case $1 in
    --sops) enc=sops ;;
    --plaintext) enc=plain ;;
    --age)
      [ "$#" -ge 2 ] || die "--age needs a recipient"
      enc=age; recipient=$2; shift ;;
    --age-identity)
      [ "$#" -ge 2 ] || die "--age-identity needs a file"
      enc=age; identity=$2; shift ;;
    --force) force=1 ;;
    *) die "unknown option $1" ;;
  esac
  shift
done
[ -n "$enc" ] || die "choose --sops, --age <recipient> / --age-identity <file>, or --plaintext"
case $recipient in -*) die "the age recipient must not start with -" ;; esac
case $identity in -*) die "the age identity file must not start with -" ;; esac
[ "$force" -eq 0 ] || [ "$mode" = restore ] || die "--force only applies to restore"
if [ "$mode" = backup ] && [ "$enc" = age ]; then [ -n "$recipient" ] || die "backup with age needs --age <recipient>"; fi
if [ "$mode" = restore ] && [ "$enc" = age ]; then [ -n "$identity" ] || die "restore with age needs --age-identity <file>"; fi
if [ "$mode" = backup ] && [ -n "$identity" ]; then die "--age-identity is for restore"; fi
if [ "$mode" = restore ] && [ -n "$recipient" ]; then die "--age <recipient> is for backup"; fi

command -v fly >/dev/null 2>&1 || die "fly (flyctl) is required"
command -v jq >/dev/null 2>&1 || die "jq is required"
case $enc in
  sops) command -v sops >/dev/null 2>&1 || die "sops is required for --sops" ;;
  age) command -v age >/dev/null 2>&1 || die "age is required for --age" ;;
esac

umask 077

if [ "$mode" = backup ]; then
  dir=$(dirname "$file")
  [ -d "$dir" ] || die "directory $dir does not exist"
  [ ! -e "$file" ] || die "$file already exists; refusing to overwrite it (move it away first)"
  tmp_out=$(mktemp "$dir/.enroll-backup.XXXXXX") || die "cannot create a temporary file in $dir"
  trap 'rm -f "$tmp_out"' EXIT INT TERM

  # The export is held in a shell variable and piped into the encryption tool: no unencrypted file is ever written.
  # Only the (encrypted) result gets a temporary file, next to the destination so the final rename is atomic.
  json=$(fly ssh console -a "$app" -C "node-control enroll-export" 2>/dev/null) \
    || die "enroll-export failed (is the app running with ENROLL_REDIS=1, and flyctl logged in?)"
  printf '%s' "$json" | jq -e '(.devices | type == "array") and ((.approvals | type) as $t | $t == "array" or $t == "null") and (.seq | type == "number")' >/dev/null 2>&1 \
    || die "the export is not the expected JSON; nothing was written"
  devices=$(printf '%s' "$json" | jq '.devices | length')
  approvals=$(printf '%s' "$json" | jq '(.approvals // []) | length')
  seq=$(printf '%s' "$json" | jq '.seq')
  [ "$devices" -gt 0 ] || die "the export holds no devices (is this the right app, and is Redis populated?); nothing was written"
  marker=$(printf '%s' "$json" | jq -r '[.devices[].uuids | to_entries[].value] | first // empty')

  case $enc in
    sops) printf '%s' "$json" | sops -e --input-type json --output-type json --filename-override "$file" /dev/stdin >"$tmp_out" 2>/dev/null \
            || die "sops could not encrypt (check .sops.yaml creation rules for $file)" ;;
    age) printf '%s' "$json" | age -r "$recipient" -o "$tmp_out" 2>/dev/null || die "age could not encrypt" ;;
    plain) printf '%s\n' "$json" >"$tmp_out" || die "cannot write" ;;
  esac
  json=
  [ -s "$tmp_out" ] || die "the encrypted output is empty; nothing was written"
  # A device UUID must not appear in an encrypted result (sops leaves key names visible, never values).
  if [ "$enc" != plain ] && [ -n "$marker" ] && grep -qF -- "$marker" "$tmp_out" 2>/dev/null; then
    die "the output still contains a device UUID in clear; nothing was written"
  fi
  mv "$tmp_out" "$file" || die "cannot move the result to $file"
  echo "backup written: $file ($devices devices, $approvals approvals, id counter $seq; encryption: $enc)"
  exit 0
fi

# restore
[ -f "$file" ] || die "$file does not exist"
case $enc in
  sops) dec() { sops -d "$file"; } ;;
  age) dec() { age -d -i "$identity" "$file"; } ;;
  plain) dec() { cat "$file"; } ;;
esac
check=$(dec 2>/dev/null) || die "cannot decrypt $file"
printf '%s' "$check" | jq -e '(.devices | type == "array") and (.seq | type == "number")' >/dev/null 2>&1 \
  || die "$file does not decrypt to an enrolment export"
echo "restoring $(printf '%s' "$check" | jq '.devices | length') devices into $app (force=$force) ..." >&2
cmd="node-control enroll-import -json -"
[ "$force" -eq 0 ] || cmd="$cmd -force"
printf '%s' "$check" | fly ssh console -a "$app" -C "$cmd"
rc=$?
check=
exit "$rc"

#!/usr/bin/env bash
# Run Terraform against a sops-encrypted state file that lives in git.
#
#   tf-run.sh plan  [terraform args...]
#   tf-run.sh apply [terraform args...]
#   tf-run.sh save                       # re-encrypt + commit a plaintext state left by a failed run
#
# Run it from the ROOT OF THE REPO THAT OWNS THE STATE (for a private site repo that
# vendors headlamp as a submodule: `vendor/headlamp/scripts/tf-run.sh apply`). The repo
# root is the git toplevel of the current directory, not of this script.
#
# Environment:
#   SOPS_AGE_KEY    required; the age private key (the Makefile fetches it from 1Password)
#   TF_DIR          terraform root, relative to the repo root         (default: terraform)
#   ALLOW_DIRTY=1   apply even though $TF_DIR/ has uncommitted changes
#   ALLOW_STALE=1   apply even though the upstream branch is ahead / unreachable
#
# What it protects against (state in git has no locking and no versioning of its own):
#   * stale state       apply refuses if $TF_DIR/ is dirty or the checkout is behind its
#                       upstream, and the new state is committed (and pushed, if there is an
#                       upstream) right after every run that changed it -- even a failed,
#                       partially applied one.
#   * lost state        the plaintext state is re-encrypted even when terraform fails or is
#                       interrupted (Ctrl-C is handed to terraform; this script keeps going).
#                       The encrypted copy is round-trip verified before it replaces the old
#                       one, and if anything goes wrong the plaintext is KEPT (never deleted)
#                       so `tf-run.sh save` can finish the job. The next run refuses to start
#                       while a leftover plaintext state exists.
#   plaintext leaks     plaintext state/secrets only exist while terraform runs, and are
#                       removed on every exit path.
#   diff noise          the state is re-encrypted only when terraform actually changed it
#                       (a plain `plan` leaves the encrypted file byte-identical).
#
# Encrypting goes through a temp dir OUTSIDE the repo and is copied in as the last step:
# sops writing straight into a project directory was intermittently racing with whatever
# watches it and producing a valid-but-empty file.
set -euo pipefail

die()  { echo "tf-run: $*" >&2; exit 1; }
warn() { echo "tf-run: warning: $*" >&2; }
usage() { echo "usage: $0 plan|apply [terraform args...] | save" >&2; exit 2; }

mode="${1:-}"
[ -n "$mode" ] || usage
shift
case "$mode" in plan | apply | save) ;; *) usage ;; esac

ROOT="${HEADLAMP_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null)}" || die "run this from inside the git repo that owns the state"
cd "$ROOT"

TF_DIR="${TF_DIR:-terraform}"
[ -d "$TF_DIR" ] || die "no $TF_DIR/ directory under $ROOT (set TF_DIR)"

STATE="$TF_DIR/terraform.tfstate"
STATE_ENC="$TF_DIR/terraform.tfstate.enc.json"
SECRETS="$TF_DIR/secrets.auto.tfvars.json"
SECRETS_ENC="$TF_DIR/secrets.auto.tfvars.enc.json"
SA_KEY="$TF_DIR/yandex-sa-terraform-key.json"
SA_KEY_ENC="$TF_DIR/yandex-sa-terraform-key.json.enc.json"
PLAINTEXT="$STATE $STATE.backup $STATE.tmp $SECRETS $SECRETS.tmp $SA_KEY $SA_KEY.tmp"

command -v sops >/dev/null || die "sops not found"
command -v python3 >/dev/null || die "python3 not found"
[ -n "${SOPS_AGE_KEY:-}" ] || die "SOPS_AGE_KEY is not set (run through the Makefile, which fetches it)"
[ -f .sops.yaml ] || die ".sops.yaml not found in $ROOT"
# Recipients come from .sops.yaml so a second (backup) age key added there is used here too.
RECIPIENT_LINES="$(sed -nE 's/^[[:space:]]*(-[[:space:]]+)?age:[[:space:]]*//p' .sops.yaml | tr -d '[:blank:]')"
[ -n "$RECIPIENT_LINES" ] || die "no 'age:' recipients found in .sops.yaml"
[ "$(echo "$RECIPIENT_LINES" | sort -u | wc -l | tr -d '[:space:]')" = 1 ] \
  || die ".sops.yaml rules list different age recipients; tf-run.sh encrypts to ONE set -- make them identical"
RECIPIENTS="$(echo "$RECIPIENT_LINES" | head -1)"

KEEP_PLAINTEXT=0
WORK=""
# shellcheck disable=SC2329  # invoked via trap
cleanup() {
  if [ "$KEEP_PLAINTEXT" = 1 ]; then
    # shellcheck disable=SC2086
    rm -f $SECRETS $SECRETS.tmp $SA_KEY $SA_KEY.tmp   # keep ONLY the state
  else
    # shellcheck disable=SC2086
    rm -f $PLAINTEXT
  fi
  [ -z "$WORK" ] || rm -rf "$WORK"
}

# A plaintext state left over from a killed run may be newer than the encrypted one.
if [ "$mode" != save ]; then
  for f in $PLAINTEXT; do
    [ -e "$f" ] || continue
    die "leftover plaintext '$f' from an interrupted run -- it may hold state that was never saved.
       Run '$0 save' to re-encrypt and commit it, or delete it if you are sure it is stale."
  done
elif [ ! -s "$STATE" ]; then
  die "nothing to save: no plaintext $STATE"
fi

# Installed only AFTER the leftover check above: refusing to start must never delete the
# very file that check is protecting.
trap cleanup EXIT

# sops re-serialises JSON (indentation/key layout), so decrypted != original BYTES even when
# nothing changed; compare the parsed documents, never the raw bytes.
same_json() { python3 -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1])) == json.load(open(sys.argv[2])) else 1)' "$1" "$2"; }

serial_of() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["serial"])' "$1"; }

preflight_git() {
  local strict="$1" dirty upstream behind
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "not a git work tree"
  dirty="$(git status --porcelain -- "$TF_DIR")"
  if [ -n "$dirty" ]; then
    if [ "$strict" = 1 ] && [ "${ALLOW_DIRTY:-0}" != 1 ]; then
      echo "$dirty" >&2
      die "uncommitted changes under $TF_DIR/ -- commit or stash them first (or ALLOW_DIRTY=1)"
    fi
    warn "uncommitted changes under $TF_DIR/"
  fi
  if upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null)"; then
    if ! git fetch --quiet 2>/dev/null; then
      [ "$strict" = 1 ] && [ "${ALLOW_STALE:-0}" != 1 ] && die "cannot reach the remote to check for newer state (ALLOW_STALE=1 to override)"
      warn "cannot reach the remote; state freshness unchecked"
    fi
    behind="$(git rev-list --count 'HEAD..@{u}' 2>/dev/null || echo 0)"
    if [ "$behind" != 0 ]; then
      [ "$strict" = 1 ] && [ "${ALLOW_STALE:-0}" != 1 ] && die "this checkout is $behind commit(s) behind $upstream -- pull first (the state may be newer there)"
      warn "this checkout is $behind commit(s) behind $upstream"
    fi
  else
    warn "no upstream configured: the only copy of the encrypted state is on this machine -- back it up"
  fi
}

decrypt() { # decrypt <enc> <out>
  [ -f "$1" ] || return 0
  sops -d "$1" > "$2.tmp"
  mv "$2.tmp" "$2"
}

# Encrypt the plaintext state, verify the round trip, install it, commit it.
save_state() { # save_state <rc> <label>
  local rc="$1" label="$2" serial msg
  [ -n "$WORK" ] || WORK="$(mktemp -d)"
  KEEP_PLAINTEXT=1                                   # until the encrypted copy is verified
  serial="$(serial_of "$STATE")" || die "state is not valid JSON; plaintext kept at $STATE"
  cp "$STATE" "$WORK/stage.json"
  sops -e --config /dev/null --age "$RECIPIENTS" -i "$WORK/stage.json" || die "encrypting failed; plaintext state kept at $STATE ('$0 save' to retry)"
  sops -d "$WORK/stage.json" > "$WORK/roundtrip.json" || die "cannot decrypt the freshly encrypted copy; plaintext kept at $STATE"
  same_json "$WORK/roundtrip.json" "$STATE" || die "encrypted copy does not decrypt back to the state; plaintext kept at $STATE"
  cp "$WORK/stage.json" "$STATE_ENC.new"
  cmp -s "$WORK/stage.json" "$STATE_ENC.new" || die "copy into repo corrupted; plaintext kept at $STATE"
  mv "$STATE_ENC.new" "$STATE_ENC"
  KEEP_PLAINTEXT=0
  echo "tf-run: encrypted state saved (serial $serial)"

  msg="chore(terraform): state after $label (serial $serial)"
  [ "$rc" = 0 ] || msg="$msg [terraform exited $rc -- possibly partial]"
  if git add -- "$STATE_ENC" && git commit -q -m "$msg" -- "$STATE_ENC"; then
    echo "tf-run: committed: $msg"
    if git rev-parse --abbrev-ref --symbolic-full-name '@{u}' >/dev/null 2>&1; then
      git push --quiet || warn "push failed -- push the state commit yourself, it is not backed up yet"
    fi
  else
    warn "could not commit $STATE_ENC -- commit it yourself, it is not in git yet"
  fi
}

if [ "$mode" = save ]; then
  save_state 0 "manual save"
  exit 0
fi

if [ "$mode" = apply ]; then preflight_git 1; else preflight_git 0; fi

WORK="$(mktemp -d)"
decrypt "$STATE_ENC" "$STATE"
decrypt "$SECRETS_ENC" "$SECRETS"
decrypt "$SA_KEY_ENC" "$SA_KEY"
[ -s "$STATE" ] && cp "$STATE" "$WORK/state.before.json"

# Ctrl-C/TERM reach terraform directly (same process group); this script must outlive it so
# the state gets saved. A handler (unlike `trap ''`) is reset in children, so terraform still
# sees the signal.
trap 'echo "tf-run: interrupt received -- waiting for terraform to stop, then saving state" >&2' INT TERM

set +e
( cd "$TF_DIR" && terraform init -input=false && terraform "$mode" "$@" )
rc=$?
set -e

if [ -s "$STATE" ]; then
  if [ -f "$WORK/state.before.json" ] && same_json "$STATE" "$WORK/state.before.json"; then
    echo "tf-run: state unchanged; encrypted state left as is"
  else
    save_state "$rc" "$mode"
  fi
elif [ -f "$STATE_ENC" ]; then
  warn "terraform left no state file; encrypted state untouched"
fi

exit "$rc"

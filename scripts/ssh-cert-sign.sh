#!/usr/bin/env bash
# Make a throwaway SSH key and a short-lived user certificate for it, for a CI job that runs Ansible against hosts
# configured by the ssh-ca role (ansible/roles/ssh-ca/README.md).
#
#   ssh-cert-sign.sh --out DIR --id KEY_ID [--principals deploy] [--validity +30m] [--source-address CIDR[,CIDR]]
#                    [--host-ca-pub FILE --host-pattern PATTERN]
#
# The user CA private key comes from the environment variable SSH_CA_KEY (OpenSSH private key text, no passphrase).
# It is loaded into a private ssh-agent that this script starts and stops; it is never written to disk.
# Alternatively, set SSH_CA_PUB to the CA public key file and leave SSH_CA_KEY unset: the key must then be in the
# ssh-agent of SSH_AUTH_SOCK.
#
# Output in DIR (created, mode 0700):
#   id_ed25519, id_ed25519-cert.pub   the key and its certificate (OpenSSH loads the -cert.pub file next to the key)
#   known_hosts                       with --host-ca-pub: "@cert-authority PATTERN <host CA key>"
# Then point Ansible at it, for example:
#   ANSIBLE_PRIVATE_KEY_FILE=DIR/id_ed25519
#   ANSIBLE_SSH_ARGS="-o UserKnownHostsFile=DIR/known_hosts -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes"
#
# The certificate allows no agent, port or X11 forwarding. --validity accepts +Nm or +Nh up to 24 hours.
set -euo pipefail

die() { echo "ssh-cert-sign: $*" >&2; exit 1; }
usage() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

out="" id="" principals="deploy" validity="+30m" source="" host_ca_pub="" host_pattern=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="${2:-}"; shift 2 ;;
    --id) id="${2:-}"; shift 2 ;;
    --principals) principals="${2:-}"; shift 2 ;;
    --validity) validity="${2:-}"; shift 2 ;;
    --source-address) source="${2:-}"; shift 2 ;;
    --host-ca-pub) host_ca_pub="${2:-}"; shift 2 ;;
    --host-pattern) host_pattern="${2:-}"; shift 2 ;;
    -h | --help) usage ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ -n "$out" ] || die "--out is required"
[ -n "$id" ] || die "--id is required (for example gha-\$GITHUB_RUN_ID; it shows in the auth log of each host)"
[[ "$id" =~ ^[A-Za-z0-9@:._/-]+$ ]] || die "--id: use letters, digits and @:._/- only"
[[ "$principals" =~ ^[A-Za-z0-9_.-]+(,[A-Za-z0-9_.-]+)*$ ]] || die "--principals: a comma-separated list of names"
[ -z "$source" ] || [[ "$source" =~ ^[0-9A-Fa-f.:/]+(,[0-9A-Fa-f.:/]+)*$ ]] || die "--source-address: a comma-separated list of CIDRs"
if [[ "$validity" =~ ^\+([0-9]+)([mh])$ ]]; then
  n="${BASH_REMATCH[1]}"; unit="${BASH_REMATCH[2]}"
  minutes=$n; [ "$unit" = h ] && minutes=$((n * 60))
  [ "$minutes" -ge 1 ] && [ "$minutes" -le 1440 ] || die "--validity: from +1m to +24h"
else
  die "--validity: +Nm or +Nh (for example +30m)"
fi
if [ -n "$host_ca_pub" ] || [ -n "$host_pattern" ]; then
  [ -n "$host_ca_pub" ] && [ -n "$host_pattern" ] || die "--host-ca-pub and --host-pattern go together"
  [ -f "$host_ca_pub" ] || die "no file $host_ca_pub"
  [[ "$host_pattern" =~ ^[^[:space:]]+$ ]] || die "--host-pattern: no spaces"
fi
command -v ssh-keygen >/dev/null || die "ssh-keygen not found"

agent_pid=""
cleanup() {
  [ -z "$agent_pid" ] || kill "$agent_pid" 2>/dev/null || true
  [ -z "${ca_pub_tmp:-}" ] || rm -f "$ca_pub_tmp"
}
trap cleanup EXIT

if [ -n "${SSH_CA_KEY:-}" ]; then
  command -v ssh-agent >/dev/null && command -v ssh-add >/dev/null || die "ssh-agent and ssh-add are needed with SSH_CA_KEY"
  eval "$(ssh-agent -s)" >/dev/null
  agent_pid="$SSH_AGENT_PID"
  printf '%s\n' "$SSH_CA_KEY" | ssh-add -q - 2>/dev/null || die "SSH_CA_KEY is not a usable private key (a passphrase is not supported)"
  [ "$(ssh-add -L | wc -l | tr -d ' ')" = 1 ] || die "the private agent should hold exactly one key"
  ca_pub_tmp="$(mktemp)"
  ssh-add -L > "$ca_pub_tmp"
  ca_pub="$ca_pub_tmp"
elif [ -n "${SSH_CA_PUB:-}" ]; then
  [ -f "$SSH_CA_PUB" ] || die "no file $SSH_CA_PUB"
  [ -n "${SSH_AUTH_SOCK:-}" ] || die "SSH_CA_PUB needs the CA private key in the ssh-agent of SSH_AUTH_SOCK"
  ca_pub="$SSH_CA_PUB"
else
  die "set SSH_CA_KEY (the CA private key) or SSH_CA_PUB (with the key in ssh-agent)"
fi

mkdir -p "$out"
chmod 700 "$out"
rm -f "$out/id_ed25519" "$out/id_ed25519.pub" "$out/id_ed25519-cert.pub"
ssh-keygen -q -t ed25519 -N '' -C "$id" -f "$out/id_ed25519"

opts=(-O clear -O permit-pty)
[ -z "$source" ] || opts+=(-O "source-address=$source")
# Serial: seconds since the epoch, so certificates of one CA sort by issue time in the auth log.
ssh-keygen -q -U -s "$ca_pub" -I "$id" -n "$principals" -V "-5m:$validity" -z "$(date +%s)" "${opts[@]}" "$out/id_ed25519.pub" \
  || die "signing failed"

if [ -n "$host_ca_pub" ]; then
  printf '@cert-authority %s %s\n' "$host_pattern" "$(head -n 1 "$host_ca_pub")" > "$out/known_hosts"
fi

ssh-keygen -L -f "$out/id_ed25519-cert.pub" | grep -E '^[[:space:]]*(Key ID|Serial|Valid):' | sed 's/^[[:space:]]*/  /'
echo "  Principals: $principals"
echo "key: $out/id_ed25519"

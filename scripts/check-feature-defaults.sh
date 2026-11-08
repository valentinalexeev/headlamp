#!/usr/bin/env bash
# Fail when an Ansible role default turns a feature on (`<name>_enabled: true|yes`) and the
# role/variable pair is not in scripts/feature-defaults.allow (#200).
# Usage: check-feature-defaults.sh [ROLES_DIR] [ALLOW_FILE]
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
roles=${1:-$here/../ansible/roles}
allow=${2:-$here/feature-defaults.allow}
rc=0
for f in "$roles"/*/defaults/*.yml "$roles"/*/defaults/*.yaml; do
  [ -f "$f" ] || continue
  role=$(basename "$(dirname "$(dirname "$f")")")
  while IFS= read -r var; do
    if ! grep -qxE "${role}[[:space:]]+${var}" "$allow"; then
      echo "ERROR: $f: $var defaults to on. Before 1.0.0 a new feature is off by default." >&2
      echo "       Set it to false, or add '$role $var' to $(basename "$allow") with a preflight that names the variable." >&2
      rc=1
    fi
  done < <(sed -nE 's/^([A-Za-z0-9_]+_enabled):[[:space:]]*"?(true|True|yes|on)"?[[:space:]]*(#.*)?$/\1/p' "$f")
done
[ "$rc" -eq 0 ] && echo "feature defaults: OK"
exit "$rc"

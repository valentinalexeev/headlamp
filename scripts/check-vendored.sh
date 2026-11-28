#!/usr/bin/env bash
# #429, ADR 0005 section 5: vendored third-party files must be the pinned release, byte for byte. Each directory
# with vendored files has a manifest VENDORED: one line per file, "<file> <version or -> <sha256> <source>". The
# check fails on a hash that differs, a version that the file does not declare (version="X.Y.Z"), a listed file that
# is missing, and a file in the directory that the manifest does not list. make vulncheck and Dependabot do not see
# these files, so a change to them must go through the manifest and its review.
# Usage: scripts/check-vendored.sh [repo dir]. Exit 0 = clean, 1 = a finding.
set -euo pipefail
cd "${1:-$(git rev-parse --show-toplevel)}"
fail=0
found=0

while IFS= read -r manifest; do
  found=1
  dir=$(dirname "$manifest")
  listed=()
  while read -r file version sum _; do
    case "$file" in '' | '#'*) continue ;; esac
    listed+=("$file")
    path="$dir/$file"
    if [ ! -f "$path" ]; then
      echo "MISSING  $path (listed in $manifest)"; fail=1; continue
    fi
    got=$(sha256sum "$path" | cut -d' ' -f1)
    if [ "$got" != "$sum" ]; then
      echo "CHANGED  $path: sha256 $got, want $sum"; fail=1; continue
    fi
    if [ "$version" != "-" ] && ! grep -qF "version=\"$version\"" "$path"; then
      echo "VERSION  $path does not declare version=\"$version\""; fail=1; continue
    fi
    echo "ok       $path ${version/#-/}"
  done < "$manifest"
  for path in "$dir"/*; do
    name=$(basename "$path")
    [ "$name" = VENDORED ] && continue
    if [[ " ${listed[*]} " != *" $name "* ]]; then
      echo "UNLISTED $path is not in $manifest"; fail=1
    fi
  done
done < <(git ls-files '*/VENDORED' 'VENDORED')

[ "$found" = 1 ] || { echo "no VENDORED manifest is tracked"; exit 1; }
exit $fail

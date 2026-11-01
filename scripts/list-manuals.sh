#!/usr/bin/env bash
# Lists the manuals that docs/writing-manuals.md covers, one per line. With BASE, only those changed since BASE.
# CLAUDE.md is left out: only one section of it is a manual, and a script cannot check part of a file.
set -euo pipefail

if [ $# -gt 0 ]; then
	files=$(git diff --name-only --diff-filter=d "$1" HEAD)
else
	files=$(git ls-files)
fi
grep -E '(^|/)README\.md$|^docs/.*\.md$|^VERSIONING\.md$' <<<"$files" | grep -v '^docs/superpowers/' || true

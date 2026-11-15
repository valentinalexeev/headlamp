# Shared setup for the bats tests (ADR 0004). Load it with `load ../helpers` (or the right depth).
# It loads bats-support and bats-assert from the pinned submodules, and sets REPO to the checkout root.

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO
load "$REPO/test/bats/bats-support/load"
load "$REPO/test/bats/bats-assert/load"

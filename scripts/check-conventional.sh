#!/usr/bin/env bash
# Checks that a title is a Conventional Commit: type(scope)!: subject, with the scope and ! optional.
# usage: check-conventional.sh "feat(api): add a thing"
set -euo pipefail

title=${1:-}
if [[ ! $title =~ ^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9._/-]+\))?!?:\ [^[:space:]] ]]; then
	echo "not a Conventional Commit title: $title" >&2
	echo "expected: type(scope): subject, with ! before the colon for a breaking change (see VERSIONING.md)" >&2
	exit 1
fi

#!/usr/bin/env bats
# Tests for scripts/changelog.sh and scripts/changelog.py (#455): check, preview, release, forward and the PR gate.
# The fixtures are written by setup() into the test's temp directory.

setup() {
	load ../helpers
	script="$REPO/scripts/changelog.sh"
	T="$BATS_TEST_TMPDIR"
	mkdir -p "$T/frag"
	cat >"$T/CHANGELOG.md" <<'EOF'
# Changelog

Intro.

## 0.1.0 - 2026-01-01

### Added

- Old.
EOF
	cat >"$T/UPGRADING.md" <<'EOF'
# Upgrading a site

Intro.

## 0.1.0

No action required.
EOF
	export CHANGELOG_FILE="$T/CHANGELOG.md" UPGRADING_FILE="$T/UPGRADING.md" CHANGELOG_DIR="$T/frag"
}

frag() { # name, then the file content on stdin
	cat >"$T/frag/$1"
}

good() {
	frag 20-new-thing.md <<'EOF'
---
issue: 20
type: feature
section: Added
status: done
title: New thing
components: [node-control]
pr: 99
---
## Summary

First line.
Second line.
EOF
	frag 10-old-knob.md <<'EOF'
---
issues: [10, 12]
type: removal
section: Removed
status: done
breaking: true
title: Old knob removed.
---
## Summary

The knob is gone.

## Upgrade

1. Remove `OLD_KNOB` from the site settings.
EOF
	frag 5-sec.md <<'EOF'
---
issue: 5
type: security
section: Security
status: done
title: "Quoted #title"
---
## Summary

Safer.
EOF
	frag 30-later.md <<'EOF'
---
issue: 30
type: feature
section: Added
status: in-progress
title: Later
---
## Summary

Not yet.
EOF
	frag 31-gone.md <<'EOF'
---
issue: 31
type: fix
section: Fixed
status: dropped
title: Gone
---
## Summary

Never.
EOF
}

@test "check: valid fragments pass" {
	good
	run "$script" check
	assert_success
	assert_output --partial "ok: 5 fragment(s)"
}

@test "check: README and files with _ are not fragments" {
	cp "$REPO/changelog.d/_template.md" "$REPO/changelog.d/README.md" "$T/frag/"
	run "$script" check
	assert_success
	assert_output --partial "ok: 0 fragment(s)"
}

@test "check: each problem is reported" {
	frag 7-bad.md <<'EOF'
---
issue: -3
type: thing
section: Added
status: done
breaking: true
color: red
---
## Notes

text
EOF
	frag Bad_Name.md <<'EOF'
no frontmatter
EOF
	run "$script" check
	assert_failure 1
	assert_output --partial "7-bad.md: unknown key 'color'"
	assert_output --partial "7-bad.md: issue/issues must be positive integers"
	assert_output --partial "7-bad.md: type: 'thing' is not one of"
	assert_output --partial "7-bad.md: missing required key 'title'"
	assert_output --partial "7-bad.md: unknown section '## Notes'"
	assert_output --partial "7-bad.md: missing required '## Summary'"
	assert_output --partial "7-bad.md: breaking: true needs a '## Upgrade' section"
	assert_output --partial "Bad_Name.md: must start with a '---' frontmatter line"
}

@test "check: the file name must match the issue, and the summary has at most 8 lines" {
	frag 8-wrong.md <<'EOF'
---
issue: 9
type: fix
section: Fixed
status: done
title: T
---
## Summary

one
EOF
	run "$script" check
	assert_failure 1
	assert_output "8-wrong.md: file name issue 8 is not in issue/issues"
	printf -- '---\nissue: 9\ntype: fix\nsection: Fixed\nstatus: done\ntitle: T\n---\n## Summary\n\n1\n2\n3\n4\n5\n6\n7\n8\n9\n' \
		>"$T/frag/8-wrong.md"
	mv "$T/frag/8-wrong.md" "$T/frag/9-long.md"
	run "$script" check
	assert_failure 1
	assert_output "9-long.md: '## Summary' has 9 lines, max. 8"
}

expected_changelog='# Changelog

Intro.

## 0.2.0 - 2026-02-03

### Added

- Existing added line (#2).

- node-control (#20): **New thing.** First line.
  Second line.

### Removed

- **BREAKING** (#10, #12): **Old knob removed.** The knob is gone.

### Fixed

- Existing fix line (#1).

### Security

- (#5): **Quoted #title.** Safer.

## 0.1.0 - 2026-01-01

### Added

- Old.'

expected_upgrading='# Upgrading a site

Intro.

## 0.2.0

### Old knob removed (#10, #12)

1. Remove `OLD_KNOB` from the site settings.

## 0.1.0

No action required.'

with_unreleased() {
	cat >"$T/CHANGELOG.md" <<'EOF'
# Changelog

Intro.

## Unreleased

### Fixed

- Existing fix line (#1).

### Added

- Existing added line (#2).

## 0.1.0 - 2026-01-01

### Added

- Old.
EOF
}

@test "release: existing Unreleased lines kept, fragments merged in Keep a Changelog order, done only" {
	good
	with_unreleased
	run "$script" release 0.2.0 2026-02-03
	assert_success
	assert_output --partial "released 3 fragment(s) into 0.2.0; 1 dropped deleted; 1 planned/in-progress kept"
	assert_equal "$(cat "$T/CHANGELOG.md")" "$expected_changelog"
	assert_equal "$(cat "$T/UPGRADING.md")" "$expected_upgrading"
	assert_equal "$(ls "$T/frag")" "30-later.md"
}

@test "preview: shows both sections and changes nothing" {
	good
	with_unreleased
	before=$(cat "$T/CHANGELOG.md")
	run "$script" preview
	assert_success
	assert_output --partial "- node-control (#20): **New thing.** First line."
	assert_output --partial "### Old knob removed (#10, #12)"
	assert_equal "$(cat "$T/CHANGELOG.md")" "$before"
	assert_equal "$(ls "$T/frag" | wc -l | tr -d ' ')" 5
}

@test "release: no Unreleased heading, and no Upgrade part gives 'No action required.'" {
	frag 5-sec.md <<'EOF'
---
issue: 5
type: security
section: Security
status: done
title: S
---
## Summary

Safer.
EOF
	run "$script" release 0.2.0 2026-02-03
	assert_success
	run sed -n '5,9p' "$T/CHANGELOG.md"
	assert_output "## 0.2.0 - 2026-02-03

### Security

- (#5): **S.** Safer."
	run sed -n '5,7p' "$T/UPGRADING.md"
	assert_output "## 0.2.0

No action required."
}

@test "release: refuses a version twice, a bad version and a bad date" {
	run "$script" release 0.1.0 2026-02-03
	assert_failure
	assert_output --partial "already has a section for 0.1.0"
	run "$script" release 'v1;x' 2026-02-03
	assert_failure
	run "$script" release 0.2.0 03.02.2026
	assert_failure
}

@test "release: an invalid fragment stops the release before any write" {
	frag 7-bad.md <<'EOF'
---
issue: 7
---
EOF
	before=$(cat "$T/CHANGELOG.md")
	run "$script" release 0.2.0 2026-02-03
	assert_failure 1
	assert_equal "$(cat "$T/CHANGELOG.md")" "$before"
}

# A throwaway repository with main and maint/0.1, for forward and pr-gate.
repo() {
	unset CHANGELOG_FILE UPGRADING_FILE CHANGELOG_DIR
	g="$T/git"
	mkdir -p "$g/scripts" "$g/changelog.d"
	cp "$REPO/scripts/changelog.sh" "$REPO/scripts/changelog.py" "$g/scripts/"
	cp "$T/CHANGELOG.md" "$T/UPGRADING.md" "$g/"
	gitc() { git -C "$g" -c user.name=t -c user.email=t@t "$@"; }
	gitc init -q -b main
	gitc add -A
	gitc commit -qm base
}

@test "forward: copies the patch sections from maint and deletes the released fragments on main" {
	repo
	# a fix with its fragment on main and, as a backport, on maint/0.1
	gitc checkout -q -b maint/0.1
	cp "$T/frag/"* "$g/changelog.d/" 2>/dev/null || true
	cat >"$g/changelog.d/40-fix.md" <<'EOF'
---
issue: 40
type: fix
section: Fixed
status: done
title: A fix
---
## Summary

Fixed.

## Upgrade

Check the thing.
EOF
	gitc add -A
	gitc commit -qm "fix: a fix"
	gitc checkout -q main
	gitc checkout -q maint/0.1 -- changelog.d/40-fix.md
	cat >"$g/changelog.d/50-feat.md" <<'EOF'
---
issue: 50
type: feature
section: Added
status: done
title: A feature
---
## Summary

New.
EOF
	gitc add -A
	gitc commit -qm "feat: a feature"
	# the patch release on maint/0.1, through a release PR: the merge repeats the subject in its body
	gitc checkout -q -b release/0.1.1 maint/0.1
	(cd "$g" && scripts/changelog.sh release 0.1.1 2026-02-01 >/dev/null)
	gitc add -A
	gitc commit -qm "chore(release): v0.1.1"
	gitc checkout -q maint/0.1
	gitc merge -q --no-ff -m "Merge pull request #9 from release/0.1.1" -m "chore(release): v0.1.1" release/0.1.1
	gitc checkout -q main
	run bash -c "cd '$g' && scripts/changelog.sh forward 0.1.1 maint/0.1"
	assert_success
	assert_output --partial "deleted 1 fragment(s) that 0.1.1 released"
	assert_equal "$(ls "$g/changelog.d")" "50-feat.md"
	run grep -n '^## ' "$g/CHANGELOG.md"
	assert_output "5:## 0.1.1 - 2026-02-01
11:## 0.1.0 - 2026-01-01"
	run grep -c 'Check the thing.' "$g/UPGRADING.md"
	assert_output 1
	# a second run changes nothing
	run bash -c "cd '$g' && scripts/changelog.sh forward 0.1.1 maint/0.1"
	assert_success
	assert_output --partial "already has 0.1.1"
}

gate() { # branch, edit command, labels; prints the exit code of the gate
	gitc checkout -q -B "$1" main
	(cd "$g" && eval "$2")
	gitc add -A
	gitc commit -qm x --allow-empty
	(cd "$g" && HEAD_REF="$1" LABELS="${3:-}" scripts/changelog.sh pr-gate main >/dev/null 2>&1) && echo 0 || echo 1
}

valid_frag='printf -- "---\nissue: 5\ntype: fix\nsection: Fixed\nstatus: done\ntitle: T\n---\n## Summary\n\nx\n" > changelog.d/5-t.md'

@test "pr-gate: code needs a fragment or the no-changelog label" {
	repo
	assert_equal "$(gate f1 'echo x > a.go')" 1
	assert_equal "$(gate f2 'echo x > a.go' 'docs,no-changelog')" 0
	assert_equal "$(gate f3 "echo x > a.go; $valid_frag")" 0
	assert_equal "$(gate f4 'mkdir -p docs test; echo x > docs/a.md; echo x > test/a.bats')" 0
}

@test "pr-gate: no direct edit of an unreleased part or a new release section" {
	repo
	assert_equal "$(gate f5 'printf "\n## Unreleased\n\n- x\n" >> UPGRADING.md')" 1
	assert_equal "$(gate f6 'sed -i.b "s/^## 0.1.0 - /## 0.1.1 - 2026-02-02\n\n## 0.1.0 - /" CHANGELOG.md; rm CHANGELOG.md.b')" 1
	assert_equal "$(gate f7 'sed -i.b "s/^- Old\./- Old, typo fixed./" CHANGELOG.md; rm CHANGELOG.md.b')" 0
	assert_equal "$(gate release/0.1.1 'sed -i.b "s/^## 0.1.0 - /## 0.1.1 - 2026-02-02\n\n## 0.1.0 - /" CHANGELOG.md; rm CHANGELOG.md.b')" 0
	assert_equal "$(gate forward/0.1.1 'printf "\n## 0.0.9\n" >> UPGRADING.md')" 0
}

---
issue: 455
type: task
section: Changed
status: done
title: Changelog fragments in changelog.d
components: [ci, scripts]
---
## Summary

A pull request now adds one file `changelog.d/<issue>-<slug>.md` instead of a line in `CHANGELOG.md` and a section in
`UPGRADING.md` (format: `changelog.d/README.md`, after agentdev#448). `scripts/changelog.sh release` makes both release
sections from the fragments; `forward` brings a patch release of `maint/X.Y` back to `main`. The workflow `changelog`
fails a pull request that changes code without a fragment (label `no-changelog` to skip) or edits these files directly.
`check-release.sh` refuses a tag while a `status: done` fragment is left.

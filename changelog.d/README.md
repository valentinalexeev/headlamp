# changelog.d: one file for each change

A pull request with a change that an operator sees adds **one fragment file** here. It does not edit
`CHANGELOG.md` or `UPGRADING.md`. Thus two open pull requests never change the same lines of these files. The
release pull request makes the release sections from the fragments.

Do not edit the unreleased part of `CHANGELOG.md` or `UPGRADING.md`, and do not add a release section to them. CI
fails a pull request that does this, except a release branch (`release/*`) and a forward branch (`forward/*`).

## The file

The name is `changelog.d/<issue>-<slug>.md`, for example `445-console-host.md`. The slug has lower-case letters,
digits and `-`. Copy [`_template.md`](_template.md). A file that starts with `_`, and this README, are not
fragments.

```markdown
---
issue: 445
type: fix
section: Fixed
status: done
title: The HTTPS console accepts its own name
components: [node-control]
pr: 451
---
## Summary

The console on port 443 accepts only `ADMIN_CONSOLE_NAME`. `ENROLL_ALLOWED_HOSTS` guards port 80 only.

## Upgrade

If you added `ADMIN_CONSOLE_NAME` to `ENROLL_ALLOWED_HOSTS`, remove it.
```

### Frontmatter

| Key | Required | Values |
|---|---|---|
| `issue` or `issues` | Yes, one of the two | A number, or a list such as `[382, 388]` |
| `type` | Yes | `feature`, `fix`, `task`, `security`, `docs`, `deprecation`, `removal` |
| `section` | Yes | `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security` (Keep a Changelog) |
| `status` | Yes | `planned`, `in-progress`, `done`, `dropped` |
| `breaking` | No, the default is `false` | `true` or `false`. With `true`, the file must have `## Upgrade`. |
| `title` | Yes | One line |
| `components` | No | A list, for example `[node-control, ansible]` |
| `pr` | No | The number of the pull request |

The frontmatter is a small part of YAML: `key: value`, lists on one line (`[a, b]`) and `#` comments. Put quotes
around a value that contains ` #`.

### Status

- `done`: the next release includes it.
- `planned` or `in-progress`: the release keeps the file. Use it for work in more than one pull request.
- `dropped`: the release deletes the file and does not include it.

### Body

- `## Summary` (required, 1 to 8 lines): the text for `CHANGELOG.md`. The entry is
  `- <components> (#<issue>): **<title>.** <summary>`. A breaking entry starts with `**BREAKING**`.
- `## Upgrade` (optional): what a site repo must do. It goes into the section of the release in `UPGRADING.md`,
  under the title of the fragment. A release with no `## Upgrade` part gets "No action required." there.

## Commands

```bash
scripts/changelog.sh check                       # validate all fragments (CI runs it)
scripts/changelog.sh preview                     # show the sections of the next release
scripts/changelog.sh release 0.22.0 2026-12-01   # release PR only: write the sections, delete the fragments
scripts/changelog.sh forward 0.21.2              # main, after a patch from maint/0.21: copy its sections
```

`release` keeps the text that is already under `## Unreleased`. It adds the entries of the `done` fragments to the
same `###` sections, in the Keep a Changelog sequence, sorted by issue number. Then it deletes the
fragments with the status `done` or `dropped`.

A patch release from `maint/X.Y` uses the fragments that are on `maint/X.Y` (the backports bring them). After the
tag, `forward` on a `forward/X.Y.Z` branch from `main` copies the `## X.Y.Z` sections of both files from
`maint/X.Y`. It also deletes on `main` the fragments that the release commit deleted on `maint/X.Y`.

## CI

The workflow `changelog` runs `check` and the bats tests (`test/scripts/changelog.bats`). It writes the preview to
the job summary. On a pull request it also fails when:

- The pull request changes code and adds or changes no fragment. Code is each file outside `docs/`, `changelog.d/`,
  `.github/`, the test directories, and `*.md`. The label `no-changelog` lets the pull request pass, for example
  for a refactor, a test or a CI change.
- The pull request changes the unreleased part of `CHANGELOG.md` or `UPGRADING.md`, or adds a release section.

`scripts/check-release.sh` refuses a tag while a fragment with the status `done` is in the tree.

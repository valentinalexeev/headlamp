# Versioning and releases

headlamp follows [Semantic Versioning 2.0.0](https://semver.org). The repository has one version, recorded in `VERSION`, and
every release publishes the whole set of images at that version.

## What the version covers

These are the public interface. Changing them incompatibly is a breaking change:

- environment variables and the config files the components read (`nodes.json`, `enroll.json`, `headscale.yaml`)
- HTTP APIs other components or scripts call: the ingress agent, funnel ingress and node-control APIs
- stored state and on-disk formats: a release must read what the previous one wrote
- image names, entrypoints and the paths an overlay adds to
- Terraform module inputs and outputs, and Ansible role variables
- command-line interfaces of the scripts in `scripts/`

The layout and wording of the console pages are not part of it. Internal Go packages are not either.

## Which number moves

| Change | Release |
|---|---|
| Breaking change to anything above | MAJOR |
| New capability that stays compatible | MINOR |
| Fix with no interface change | PATCH |

Before 1.0.0 nothing is promised beyond this: a breaking change moves MINOR, anything else may move PATCH.

Commits say which is which, using [Conventional Commits](https://www.conventionalcommits.org):
`feat` needs at least a MINOR. `!` before the colon, or a `BREAKING CHANGE:` footer, needs a MAJOR. PR titles are checked
because they usually become commit subjects.

## Rules a release must meet

`scripts/check-release.sh` enforces these when a tag is pushed, before anything is published:

- The tag is `vMAJOR.MINOR.PATCH`, or `vMAJOR.MINOR.PATCH-rc.N` for a release candidate.
- `VERSION` at that commit matches it.
- The commit is on `main`.
- It is the next version after the highest stable tag, with lower numbers reset. There is no skipping, and no fix release
  to an older line.
- It bumps at least as much as the commits since the previous release need.
- A milestone named after the version, if there is one, has no open issues.

A published version never changes. If a release is wrong, fix it in a new one: never move or delete its tag.

## Images

CI publishes `ghcr.io/valentinalexeev/headlamp/<image>`. The components are listed in `.github/components.json`, and [`docs/images.md`](docs/images.md) explains what each one does.

| Tag | Meaning | Moves? |
|---|---|---|
| `src-<hash>` | built from exactly this source | never |
| `sha-<commit>` | the image set at this commit of `main` | never |
| `main` | the newest commit of `main` | yes |
| `X.Y.Z`, `X.Y.Z-rc.N` | a release | never |
| `X.Y` | the newest release of that minor | yes |
| `X`, `latest` | the newest release of that major, and overall (from 1.0.0) | yes |

Only a component whose source changed is rebuilt. A release reuses the images `main` built, so what was tested is what
ships. Deploy by digest, or at least by `X.Y.Z`. The images hold placeholders where a site's config belongs: a deployment
adds its own in a private overlay.

## Cutting a release

1. Make a `release/X.Y.Z` branch from `develop`. In one commit `chore(release): vX.Y.Z`, set `VERSION` and move the
   `Unreleased` part of `CHANGELOG.md` under the new version. Merge it into `develop` through a PR.
2. Merge `develop` into `main` through a PR.
3. Tag the merge commit on `main` and push the tag: `git tag vX.Y.Z <commit> && git push origin vX.Y.Z`.

A hotfix starts from `main` on a `hotfix/` branch. Its PR into `main` also bumps the PATCH version and the changelog.
Tag it as above, then merge `main` back into `develop` through a PR.

CI checks the rules, tags the images and creates the GitHub release. To see whether a tag would pass before pushing it, tag
locally and run `scripts/check-release.sh vX.Y.Z`.

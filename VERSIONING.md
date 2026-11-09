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

A commit on `main` with the wrong type cannot change, because `main` history does not change. To correct it for the
release check, add a line `<full commit hash> <type>` to `.release-retype` in a PR, for example
`238a9991c1887f56f59f3ea17a11c19468e76ffe fix`. The owner approves the line like any other change.

## New features before 1.0.0

Before 1.0.0, a minor release must not change a running site that has no config changes. Thus:

- A new feature that opens a port, changes an access rule or adds a protocol is off by default. The operator sets a
  variable to turn it on.
- If a default must change, the role or app stops early, also in check mode. The error message names the variable to
  set and tells what each value does. The changelog entry and the upgrade notes name the variable.
- `make check-feature-defaults` fails when an Ansible role default `<name>_enabled` is `true` and the role and
  variable are not in `scripts/feature-defaults.allow`. Each line in that file needs such a preflight.

## Rules a release must meet

`scripts/check-release.sh` enforces these when a tag is pushed, before anything is published:

- The tag is `vMAJOR.MINOR.PATCH`, or `vMAJOR.MINOR.PATCH-rc.N` for a release candidate.
- `VERSION` at that commit matches it.
- The commit is on `main`.
- It is the next version after the highest stable tag, with lower numbers reset. There is no skipping, and no fix release
  to an older line.
- It bumps at least as much as the commits since the previous release need.
- A milestone named after the version, if there is one, has no open issues.
- [`UPGRADING.md`](UPGRADING.md) at that commit has a section for the version (`## X.Y.Z`). It lists what a site repo
  must change, or says "No action required". Rename the `## Unreleased (X.Y.Z)` section in the release PR.

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

## The tailscale version

One tailscale version applies to the whole repo (#145). The file `TAILSCALE_VERSION` holds it. These pins must have the same version:

- `ARG TSVERSION` in every Dockerfile under `apps/` (the exit-node image and the images with an embedded client);
- the `tailscale.com` module in every `go.mod` under `apps/`;
- `tailscale_version` in `ansible/roles/tailscale/defaults/main.yml`.

`scripts/check-tailscale-pins.sh` compares them, and `make check-tailscale-pins` runs it. CI runs it on each PR. To move to a new version, change all the pins in one PR. Run `go get tailscale.com@v<x.y.z>` and `go mod tidy` in each module, then run the unit tests and the end-to-end tests.

## The headscale version

The file `HEADSCALE_VERSION` holds the headscale version (#152). The default of `ARG HEADSCALE_VERSION` in
`apps/headscale/Dockerfile` and the default image of each e2e test must have the same version.
`make check-headscale-pin` compares them, and CI runs it on each PR.

## New upstream versions

Each Monday, the `upstream-versions` workflow compares `TAILSCALE_VERSION` and `HEADSCALE_VERSION` with the latest
stable releases. If a newer release exists, the workflow opens one issue for it. It does not open a second issue for the
same version.

## Charts

A release tag also publishes each chart under `charts/` to `oci://ghcr.io/valentinalexeev/headlamp/charts`. The chart
has the version of the release, and its default image tag is the same version. The `version` field in the repo is a
placeholder that CI replaces. Install a released chart:

```sh
helm install tc oci://ghcr.io/valentinalexeev/headlamp/charts/tailnet-connector --version X.Y.Z -f values-site.yaml
```

## Cutting a release

All work merges into `main` through pull requests. There is no other long-lived branch.

1. Make a `release/X.Y.Z` branch from `main`. In one commit `chore(release): vX.Y.Z`, set `VERSION` and move the
   `Unreleased` part of `CHANGELOG.md` under the new version. Merge it into `main` through a PR.
2. Tag the merge commit on `main` and push the tag: `git tag vX.Y.Z <commit> && git push origin vX.Y.Z`.

A hotfix is a normal fix on a `hotfix/` branch, followed at once by a PATCH release.

Before you tag, run `make release-preflight` on the release commit. It does these checks:

- It runs each Ansible role in check mode in a systemd container.
- It builds each image in `.github/components.json`.
- It builds each app that has an overlay in `examples/private-repo/fly/<app>/`, with the overlay on top. Then it starts
  each `[processes]` command of the `fly.toml` of that release, and of the previous release.

If a process command of the previous release fails, a site overlay breaks on upgrade. Write the change in the
changelog, then set `ALLOW_PROCESS_CHANGE=1`. `REF=vX.Y.Z` checks an older tree with the checks of today.

CI checks the rules, tags the images and creates the GitHub release. To see whether a tag would pass before pushing it, tag
locally and run `scripts/check-release.sh vX.Y.Z`.

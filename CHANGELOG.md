# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- Image publishing to GHCR for each component, rebuilt only when its source changes.
- A release policy with checks on PR titles and release tags.
- A redesigned node-control admin console, with an audit log of device, bridge and node actions.
- Bridge settings in `enroll.json`: `node` names the machine a bridge runs on, and `intermittent` makes an unreachable agent a notice instead of a risk.

### Changed

- Go modules use the repository path.

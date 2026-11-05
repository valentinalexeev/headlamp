# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- node-control `aws` provider: start, stop and status of EC2 exit nodes, found by the `Name` tag (`tag:cloud-aws`). A node in an Auto Scaling group, as `exit-node-vm/aws` creates, is stopped by scaling the group to 0 and started by scaling it to 1 (ADR 0001). Credentials come from the standard AWS chain; the README has a least-privilege IAM policy.
- `exit-node-vm/aws` tags the group and its instances `headlamp-exit-node=true` for that policy, and ignores drift in `desired_capacity` and `min_size`, so a node stopped from node-control stays stopped after `terraform apply`.

## 0.9.0 - 2026-11-05

### Added

- Image publishing to GHCR for each component, rebuilt only when its source changes.
- Publication of the documentation to the GitHub wiki when a release is tagged.
- A `ci` workflow: commit subjects, secret scan, shell lint and script tests, Terraform module validation and Ansible syntax on every PR.
- A `linux/arm64` build of the `funnel-sidecar` image. The Kubernetes examples use the published image.
- A release policy with checks on PR titles and release tags.
- A redesigned node-control admin console, with an audit log of device, bridge and node actions.
- Acknowledging an attention item in the node-control console: it leaves the queue, counts and lamp until it changes meaning, expires or clears, and Activity records who did it. Needs the enrolment store.
- Bridge settings in `enroll.json`: `node` names the machine a bridge runs on, and `intermittent` makes an unreachable agent a notice instead of a risk.

### Changed

- Go modules use the repository path.

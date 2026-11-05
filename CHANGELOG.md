# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- node-control controls Yandex nodes in a separate cloud installation: `YANDEX_API_ENDPOINT` and `YANDEX_IAM_ENDPOINT` for all nodes, or `endpoint`, `iam_endpoint` and `folder_id` in one node's `nodes.json` block. Each installation gets its own IAM token. The `exit-node-vm/yandex` README shows how to point the module at such an installation.

- funnel-ingress behind any load balancer: `FUNNEL_CLIENT_IP_HEADER` chooses the header with the client address (`X-Forwarded-For` is read as a chain) and `FUNNEL_TRUSTED_PROXIES` limits which peers may set it and `X-Forwarded-Proto`. The default keeps `Fly-Client-IP` from every peer, as before.

### Changed

- funnel-ingress ignores a client-address header that arrives twice and uses the peer address, where it used the first value before. A correct edge sets the header once, so only a forged request sees the difference.
- The wiki also publishes every component, Ansible role and Terraform module manual (for example the funnel-ingress manual as `funnel-ingress`), with a sidebar that lists all pages.

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

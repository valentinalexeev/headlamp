# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- Generic OpenStack support (#101). The module `exit-node-vm/openstack` replaces `exit-node-vm/selectel` and gains `external_network_name` (default `external-network`). `exit-node-vm/selectel` stays as a wrapper with `moved` blocks, so existing root stacks plan no change. node-control accepts the provider name `openstack` (`tag:cloud-openstack`, an `openstack` block), and `selectel` still works. Other OpenStack clouds: the operator declares them in `OPENSTACK_CLOUDS` (`NAME=https://keystone`), and a node block names one with `"cloud"`. That node signs in with the cloud's own `NAME_*` credentials. Tokens and cached ids are kept per cloud, and the Selectel password is never sent there.
- node-control `aws` provider: start, stop and status of EC2 exit nodes, found by the `Name` tag (`tag:cloud-aws`). A node in an Auto Scaling group, as `exit-node-vm/aws` creates, is stopped by scaling the group to 0 and started by scaling it to 1 (ADR 0001). Credentials come from the standard AWS chain; the README has a least-privilege IAM policy.
- `exit-node-vm/aws` tags the group and its instances `headlamp-exit-node=true` for that policy, and ignores drift in `desired_capacity` and `min_size`, so a node stopped from node-control stays stopped after `terraform apply`. The role that applies the module needs `autoscaling:CreateOrUpdateTags` for the new tag.
- Run the published images with site config mounted at runtime, with no rebuild or overlay: `HEADSCALE_CONFIG` and `HEADSCALE_POLICY_PATH` for headscale, the new `DIRECT_HOSTS_FILE` for the exit node, `NODES_CONFIG` and `ENROLL_CONFIG` for node-control. Without them the images use their placeholders, as before. See `docs/images.md`.
- `EXIT_NODE_HOSTNAME` sets the tailnet name of the exit-node image (default `ams`, as before).
- funnel-ingress behind any load balancer: `FUNNEL_CLIENT_IP_HEADER` chooses the header with the client address (`X-Forwarded-For` is read as a chain) and `FUNNEL_TRUSTED_PROXIES` limits which peers may set it and `X-Forwarded-Proto`. The default keeps `Fly-Client-IP` from every peer, as before.

### Changed

- funnel-ingress ignores a client-address header that arrives twice and uses the peer address, where it used the first value before. A correct edge sets the header once, so only a forged request sees the difference.
- The wiki also publishes every component, Ansible role and Terraform module manual (for example the funnel-ingress manual as `funnel-ingress`), with a sidebar that lists all pages.

### Deprecated

- The node-control provider name `selectel` (`tag:cloud-selectel`, a `selectel` node block) and the Terraform module `exit-node-vm/selectel`. Use `openstack` and `exit-node-vm/openstack`. They will be removed by 1.0.0.

### Security

- node-control no longer guesses the type of an answer without a `Content-Type`, and sends `X-Content-Type-Options: nosniff`. Before, an untyped answer that started with markup got the navigation shell and was sent as HTML (#108).

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

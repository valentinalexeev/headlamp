# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- The node-control console shows the headlamp version it runs (for example `v0.10.0`) at the foot of the sidebar, or in the top bar of the compact console. The start-up log has it too. The image takes it from the `VERSION` file; a local build shows `dev`.

### Changed

- Every tailscale client in the repo is now 1.104.5, and the pins agree (#145). This covers `TAILSCALE_VERSION` (was 1.102.2), the exit-node image, the Ansible `tailscale` role default (was 1.98.10) and the embedded `tailscale.com` module of node-control and funnel-ingress (was 1.98.10). 1.104.4 fixes security issues in Tailscale SSH, Serve and Funnel. The node-control and funnel-ingress images now build with Go 1.27.3, which `tailscale.com` 1.104 needs. `scripts/check-tailscale-pins.sh` (`make check-tailscale-pins`) fails when one pin differs from `TAILSCALE_VERSION`. The Ansible `tailscale` role holds the package and turns off self-update (`tailscale_pin_version`, default `true`), so the pin is the only source of the version.

### Fixed

- The `ingress-agent` Ansible role no longer fails its preflight on every bridge (#144). In 0.10.0 it still looked for the removed Python agent (`app/main.py`), so no bridge could install the Go agent. It now checks `go.mod` and `cmd/ingress-agent`. `make test-ansible-roles` runs the role checks with real ansible against the real tree.

## 0.10.0 - 2026-11-07

### Added

- Serve page in the console (#129). `/serve` shows the zone, the CA fingerprint and download, the headscale split lines, and each service with its state, keys, expiry and proxies. It has the same design and controls as the other pages. A `serve.admin` user can add a service, add a key, retire the old key and delete a service (with the name typed). A new key shows one time, with a sample `funnel-sidecar` environment. Viewers see no admin controls. Changes go to the Activity page, without the key. The Overview page shows a service with no online proxy, and a key that expires within 14 days or has expired. Services show in the search. The console demo has Serve fixtures, and a new `viewer` scenario.
- Hysteria 2 beside VLESS Reality (#110). A bridge now also serves Hysteria 2 on UDP, as a second inbound of the same xray (`hy2-in`, xray 26.9.9 or later). The ingress agent adds every credential to both inbounds, so the modes, the block of private and tailnet ranges, and the statistics are the same. Salamander obfuscation is on by default (`hy2_obfs_password`), and the certificate is Caddy's HTTP-01 certificate for the bridge host. node-control adds a `hysteria2` outbound for each mode to the sing-box and Clash profiles (`hy2_port` in `enroll.json` or `/v1/info`, password from `HY2_OBFS_<NAME>`). Each mode becomes a group of the two, so a client falls back to VLESS where UDP is blocked. A bridge that adds `hy2_port` has a new fingerprint and needs a new approval. A mixed set works: bridges with and without Hysteria 2. `make test-hysteria-e2e` runs the full flow in Docker with real sing-box and mihomo clients. Known upstream issue: stock sing-box 1.14 cannot use VLESS Reality with xray 26.9.8 or later (SagerNet/sing-box#4520); its Hysteria 2 outbound works.
- Funnel DNS automation (#127). The edge gets a certificate for each funnel host on demand (`FUNNEL_DNS=ondemand`, the new default). Before it asks the CA, it asks funnel-ingress whether the host is a live route (`/.well-known/headlamp/tls-ask`), so a client cannot make it request other names. With `FUNNEL_DNS_PROVIDER` (`route53` or `cloudflare`), node-control writes the one public record `*.<FUNNEL_PUBLIC_DOMAIN>` and puts it back if it changes. A new namespace needs no DNS or certificate step, and the funnel page shows the record status in place of the checklist. Fly.io keeps the manual checklist. `make test-funnel-tls-e2e` runs the edge against a local ACME server.
- Serve (#117): tailnet-only names with HTTPS for services behind a proxy, like `tailscale serve`. A service is one name in `SERVE_ZONE`, exact (`grafana.svc.example.com`) or a wildcard for an ingress controller (`*.apps.svc.example.com`), plus a reusable ephemeral pre-auth key that node-control creates. Every proxy of the service joins with that key. node-control learns the service from the key that headscale reports for the node, so a proxy sends no name and can get a certificate only for its own service. node-control answers DNS for the zone on its tailnet address. headscale sends it the zone with `dns.nameservers.split`, and the zone must be outside the MagicDNS base domain. Each name answers the online replicas of its service. Certificates come from your CA, or from a self-signed CA whose key is sealed with `NODE_CONTROL_STATE_KEY`. Keys rotate with two keys at a time. funnel-sidecar gets a serve mode (`FUNNEL_SERVE_URL`). `make test-serve-e2e` runs it against a real headscale. See "Serve module" in the node-control README.
- Central nodes on AWS (#41). The new module `terraform/modules/control-host/aws` builds one EC2 host with Docker. The host gets an encrypted data volume with daily snapshots and a network load balancer. The load balancer passes 443/tcp, 80/tcp and 3478/udp through and keeps the client address, so TLS ends on the host. The security group opens only those ports. The instance role gives SSM access and, if you ask for it, DNS-01 on one Route 53 zone. `examples/compose/control/` runs headscale, node-control and an optional funnel-ingress from the published images, pinned by digest. Their config is mounted read-only, and a Caddy edge gets the certificates: HTTP-01 for fixed names and Route 53 DNS-01 for funnel wildcards. The README is the guide: first start, funnel, enrolment, AWS, backups and restore, upgrades, key rotation. `make test-control-host-aws` runs plan checks against a mock provider. `make test-compose-control` runs the compose files on local Docker; a client then joins through the TLS edge and reaches node-control.
- node-control admin roles (#122). Every admin route needs a permission (a feature at `view`, `operate` or `admin`), and the server refuses to start if a route has none. Built-in roles are `viewer`, `operator` and `owner`. `ADMIN_USERS` names owners. `ADMIN_CONFIG` is a mounted file with custom roles and grants. `POLICY_ADMIN_USERS`, `FUNNEL_ADMIN_USERS` and `TASKS_ADMIN_USERS` keep working, as admin of their feature. Before, the overview, nodes and enrolment pages had no user check: anyone the tailnet ACL admitted could start and stop nodes. With an admin configured, they now need a role. With none, the console stays open as before and logs a warning; this mode is removed in 1.0.0. See "Admin roles" in the node-control README.
- node-control controls Yandex nodes in a separate cloud installation: `YANDEX_API_ENDPOINT` and `YANDEX_IAM_ENDPOINT` for all nodes, or `endpoint`, `iam_endpoint` and `folder_id` in one node's `nodes.json` block. Each installation gets its own IAM token. The `exit-node-vm/yandex` README shows how to point the module at such an installation.
- Generic OpenStack support (#101). The module `exit-node-vm/openstack` replaces `exit-node-vm/selectel` and gains `external_network_name` (default `external-network`). `exit-node-vm/selectel` stays as a wrapper with `moved` blocks, so existing root stacks plan no change. node-control accepts the provider name `openstack` (`tag:cloud-openstack`, an `openstack` block), and `selectel` still works. Other OpenStack clouds: the operator declares them in `OPENSTACK_CLOUDS` (`NAME=https://keystone`), and a node block names one with `"cloud"`. That node signs in with the cloud's own `NAME_*` credentials. Tokens and cached ids are kept per cloud, and the Selectel password is never sent there.
- node-control `aws` provider: start, stop and status of EC2 exit nodes, found by the `Name` tag (`tag:cloud-aws`). A node in an Auto Scaling group, as `exit-node-vm/aws` creates, is stopped by scaling the group to 0 and started by scaling it to 1 (ADR 0001). Credentials come from the standard AWS chain; the README has a least-privilege IAM policy.
- `exit-node-vm/aws` tags the group and its instances `headlamp-exit-node=true` for that policy, and ignores drift in `desired_capacity` and `min_size`, so a node stopped from node-control stays stopped after `terraform apply`. The role that applies the module needs `autoscaling:CreateOrUpdateTags` for the new tag.
- Run the published images with site config mounted at runtime, with no rebuild or overlay: `HEADSCALE_CONFIG` and `HEADSCALE_POLICY_PATH` for headscale, the new `DIRECT_HOSTS_FILE` for the exit node, `NODES_CONFIG` and `ENROLL_CONFIG` for node-control. Without them the images use their placeholders, as before. See `docs/images.md`.
- `EXIT_NODE_HOSTNAME` sets the tailnet name of the exit-node image (default `ams`, as before).
- funnel-ingress behind any load balancer: `FUNNEL_CLIENT_IP_HEADER` chooses the header with the client address (`X-Forwarded-For` is read as a chain) and `FUNNEL_TRUSTED_PROXIES` limits which peers may set it and `X-Forwarded-Proto`. The default keeps `Fly-Client-IP` from every peer, as before.

- Tailnet access through a bridge (#132). A bridge with `"tailnet_access": "all"` in `enroll.json` gives every client a tailnet grant. The agent then sends the tailnet addresses of these clients to the new outbound `via-tailnet`, the bridge's tailscaled SOCKS port. Other private ranges stay blocked. node-control writes one ACL rule (`ENROLL_INGRESS_TAG` to `ENROLL_TAILNET_DST`) through the policy console with `POST /enroll/tailnet-acl`, and an admin confirms it. `make test-tailnet-access-e2e` checks this for VLESS and Hysteria 2, for granted and ungranted bridges, and for allowed and denied hosts.

- Self-enrolment with OIDC (#102). People sign in with the identity provider at `https://<link_host>/me` and add their own devices. One enrolment gives a relay device and a one-time tailnet key for a headscale user of that person. The People section of the Devices page locks a person: no sign-in, all relay devices revoked at once on every bridge, and their tailnet nodes expired. The app also checks each person again at the identity provider and locks a person whom the identity provider refused or removed from the allowed groups. `make test-oidc-e2e` checks this with dex, headscale, a bridge and real clients.

### Changed

- The ingress agent is now Go (#133). It has the same HTTP API, environment variables and files, so node-control and a running xray see no change. It is one static binary with no runtime on the bridge. Measured on Linux: idle memory dropped from about 52 MB to about 12 to 14 MB, with about 16 to 18 MB at 1,000 credentials. Start-up to the first answer takes about 60 ms, and it needs about 6 MB on disk in place of 79 MB. The `ingress-agent` role builds it on the control host for the bridge's architecture (Go 1.26 or later there), installs `/usr/local/bin/ingress-agent`, and removes the old Python tree. CI checks the budget with `scripts/test/ingress-agent-budget/budget.sh`. The new variable `AGENT_LISTEN` sets the listen address, and the role's wrapper sets it to the tailnet IPv4.
- Docs no longer present Fly.io as the reference platform (#135). The README, `CLAUDE.md`, `docs/images.md` and the node-control and funnel-ingress READMEs list three ready targets for the central nodes, with none as the default. These are AWS (`terraform/modules/control-host/aws` with `examples/compose/control/`), any Docker host (the same compose files) and Fly.io. The Fly.io steps stay as one option.

- The components moved out of `apps/fly/`: `apps/headscale`, `apps/node-control`, `apps/funnel-ingress`, and `apps/fly/exit-ams` is now `apps/exit-node` (#119). The Go module paths change to `github.com/valentinalexeev/headlamp/apps/<component>`. Image names do not change. To migrate a private site repo: replace `vendor/headlamp/apps/fly/` with `vendor/headlamp/apps/` in your scripts and CI, and rename your overlay `fly/exit-ams/` to `fly/exit-node/` (`fly/deploy.sh exit-node`). The wiki page `exit-ams` is now `exit-node`.
- The manuals no longer assume Fly.io. They state what each image needs from its host, and Fly.io is the reference platform. The exit-node manual has a checked `docker run` example. In Docker, forwarding must be set with `--sysctl`, because the container cannot set it. The node-control Funnel DNS checklist shows the `fly certs` steps only when `FUNNEL_INGRESS_HOSTNAME` is a `.fly.dev` name (or unset). For another edge, it names that edge and asks for a wildcard certificate.
- funnel-ingress ignores a client-address header that arrives twice and uses the peer address, where it used the first value before. A correct edge sets the header once, so only a forged request sees the difference.
- The wiki also publishes every component, Ansible role and Terraform module manual (for example the funnel-ingress manual as `funnel-ingress`), with a sidebar that lists all pages.

### Deprecated

- The node-control provider name `selectel` (`tag:cloud-selectel`, a `selectel` node block) and the Terraform module `exit-node-vm/selectel`. Use `openstack` and `exit-node-vm/openstack`. They will be removed by 1.0.0.

### Security

- The admin JSON answers of the enrolment, funnel, policy and tasks modules keep HTML escaping (`<`, `>` and `&` as `\u003c` and so on). An answer that echoes request text cannot carry markup (code scanning alerts 6 and 7, #141).
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

# Changelog

Notable changes, per [Keep a Changelog](https://keepachangelog.com). Versions follow [VERSIONING.md](VERSIONING.md).

## Unreleased

### Added

- Fleet versions, part 1 (#195). node-control serves `GET /fleet.json` (Overview view permission). It shows the node-control version, the headscale version, the tailscale version of every tailnet node compared with the pin (current, older, newer, unknown), and the agent and xray version of every discovered bridge. It is read only. The tailscale versions come from node-control's own tailnet node, so nothing is sent to the nodes.
- ingress-agent reports its build version and its xray version in `GET /v1/info` (`version`, `xray`), and prints its version with `--version`. These fields are not in the bridge fingerprint, so an agent upgrade needs no new approval. The `ingress-agent` role stamps the version (repo VERSION plus the last commit that changed the agent) and prints "old -> new" when it changes.

## 0.15.3 - 2026-11-10

### Fixed

- node-control self-enrolment: a sign-in on a bridge front whose host is not `link_host` failed with 400 (#270). The redirect URL of the identity provider is on `link_host`, but the sign-in cookie was set on the front's host. Now `/me`, `/oidc/login` and `/i/<token>` on another host send the browser to the same page on `link_host` first, so the sign-in starts and ends there. No change at the identity provider.

## 0.15.2 - 2026-11-10

### Fixed

- The example policy `examples/compose/control/config/acl.json` now has the rules that a bridge needs, with tests (#259): `tag:ingress` to its exit node, to `autogroup:internet` and to `tag:node-control:8081`, and `tag:node-control` to `tag:ingress:9000`. Before, a bridge that followed the example could not reach the internet through its exit node: its clients connected and got no reply. The `tailscale-userspace-socks` role now sends one request through the SOCKS port after it selects the exit node, and fails with the cause when the request fails (`socks_egress_check`, default `true`). `make test-bridge-policy-e2e` checks the rules on a real headscale.
- The public enrolment paths are in one file, `enroll-paths.caddy` (#260). The `reality-ingress` front and the edge of `examples/compose/control` import it. Before, each kept its own list, and a site that had copied the example Caddyfile before 0.15.0 had no `/i/*` and `/sub/<secret>/<format>`: these links answered 404. A node-control test fails when the file and the public routes differ, and `make lint` fails when the compose copy differs from the role's file.
- `examples/compose/control`: `scripts/policy-apply.sh` applies a changed `config/acl.json` safely (#261). It checks the file in the running headscale against the live users and nodes (without the `tests` block), stops on a failure, then loads the policy with `SIGHUP`. It makes a new container when the bind mount still shows an old file, and restarts headscale when only a `tests` entry blocks the `SIGHUP` load. `--check-only` runs only the check. The README ("Change the policy") explains the traps: `docker compose up -d` does not load a changed policy, a new file can stay invisible to the container, and a policy that passes CI can stop headscale at its next restart. `make test-policy-apply-e2e` checks it.
- node-control self-enrolment: `/me` no longer makes a second headscale user for a person who already has one (#253). Before, a person who had logged in through the OIDC login of headscale got a second user `oidc-<hash>` with the same email, and headscale did not start again when the policy named the person by email. Now the first device reuses, in this order: the user with the provider ID `<OIDC_ISSUER>/<subject>`, the user `oidc-<hash>`, or the one user with the person's email when the ID token says `email_verified: true`. More than one user with that email: node-control refuses the device and says why. The OIDC database gets an `email_verified` column at start.
- node-control console: the Devices table, the tailnet grants and People show the email of a self-enrolled owner, not `oidc:<subject>` (#257). `GET /enroll/devices` gives `owner_email` for such owners. The identifier is in the cell title, and a long owner no longer widens the table. The invitation form no longer asks for a device name: the device is named after the part of the email before the `@`. The API still takes `device_name`.

### Security

- ingress-agent: xray no longer looks up client destinations with the resolver of the bridge network. `IPOnDemand` routing resolves every domain that a client goes to (also the sniffed TLS/HTTP name). Before, xray had no `dns` section, so it used the system resolver of the bridge host, and the bridge network saw every name that `exit` and `auto` clients used. A poisoned answer could also send a name to the wrong outbound. The agent now writes a `dns` section (tag `dns-internal`) and sends its queries through `via-exit`. The servers are `INGRESS_EXIT_DNS` (role variable `ingress_exit_dns`; default `https://1.1.1.1/dns-query`, `https://1.0.0.1/dns-query`). Only names on the direct geosite lists use the system resolver. A `dns` section in the base config is replaced. Clients of mode `direct` now get their addresses from these servers too, so their lookups fail while the exit node is down.

## 0.15.1 - 2026-11-09

### Fixed

- node-control: `ENROLL_PUBLIC_LISTEN` (optional, `host:port`) serves the public enrolment pages (`/e/`, `/sub/`, `/me`, `/oidc/*`, `/i/*`) also on a plain TCP address, not only on the tailnet port 8081 (#250). Before, the TLS edge of `examples/compose/control` could not reach node-control (`connection refused`), so a control host could not serve `link_host` itself. The admin app stays tailnet-only. `examples/compose/control` sets `ENROLL_PUBLIC_LISTEN=:8081` on the compose network; the port is not published.

## 0.15.0 - 2026-11-09

### Added

- node-control self-enrolment: bound invitations (#241). An administrator invites one email address on the Devices page (Invitations section, `POST /enroll/invites`, needs `admin` on `enroll`). The link `https://<link_host>/i/<token>` signs the person in and makes one device only when the verified email of the account is the invitation's (by default the ID token must have `email_verified: true`; `OIDC_INVITE_REQUIRE_VERIFIED=false` also accepts a token without the claim). A failed attempt leaves the invitation usable. Withdraw one that made no device with `POST /enroll/invites/{id}/revoke`, also while a sign-in uses it. Invitations are stored in `OIDC_DB`, as token hashes. `OIDC_INVITE_ONLY=true` turns off the "Add a device" form of `/me`. The default is `false`, so a running site does not change.
- node-control self-enrolment: an app picker (#241). The person now picks the relay app after the device is made, on `/me/devices/{id}`: one import link per app, for the platform of the browser, with the site's store links. **Add to an app** in the device list opens it again. `enroll.json` takes an optional `clients` list (the apps, their order and their store links per platform); without it every app is shown. `ENROLL_CLIENT_ICON_DIR` gives optional app icons (this repository ships none).
- node-control enrolment: Shadowrocket and Stash in the client catalog (#241), also for devices that an operator makes. `/sub/<secret>/<format>` serves any profile format (`sing-box`, `clash`, `stash`, `uri`) for every device; `/sub/<secret>` is unchanged. `stash` is the Clash profile with Stash's field names, `uri` a base64 list of `vless://` and `hysteria2://` share links.

- `exit-node-vm/openstack` (and `selectel`): `tags` and `metadata` inputs (#227). `tags` go on the instance, port, network, subnet, router, security group and floating IP; `metadata` on the instance and the boot volume. The defaults set nothing, so an existing stack plans no change.
- `exit-node-vm/openstack` (and `selectel`): `authkey_from_secret` reads the auth key at first boot from a Barbican secret, not from user data (#224). Terraform makes an application credential that can only `GET` that secret's payload (`authkey_secret_ref`, `keystone_auth_url`, optional `authkey_credential_expires_at`; output `application_credential_id`). The bool is decided at plan time, so a secret made in the same apply plans. Revoke the credential after the first boot.

### Changed

- node-control self-enrolment: the OIDC callback must come back to the browser that started the sign-in (the state is also kept in a `__Host-hl_flow` cookie). A callback URL opened in another browser gets 400 and signs nobody in.
- node-control `/me`: the "Add a device" form no longer asks for the relay app; the app picker that follows does (#241). The tailnet key shows one time, on that page. The old form field `client` is still accepted.
- `reality-ingress`: the Caddy front also proxies `/sub/<secret>/<format>`, and with `enroll_self_service: true` also `/me/devices/<id>`, `/me/icons/<app>` and `/i/<token>` (#241). Run the role on each bridge before people use the app picker: an older front answers 404 to these links. `examples/compose/control` proxies `/i/*`.
- `funnel-sidecar`: a test now shows the recovery of #238. The registration API accepts the PUT and does not answer for some rounds. Each request stops after 15 s, the sidecar tries again with backoff, and the route comes back with no restart. The behavior did not change: 0.10.0 added it (`FUNNEL_UNREACHABLE_EXIT`).

## 0.14.0 - 2026-11-09

### Added

- `exit-node-vm/openstack` (and the `selectel` wrapper) can make a Reality + Hysteria 2 bridge (#235): `extra_tcp_ingress_ports` and `extra_udp_ingress_ports` (a bridge sets `[80, 443]` and `[443]`), `advertise_exit_node = false`, and `image_id` to pin one image. The README has a bridge recipe and a Debian 13 section (Selectel `Debian 13 (Trixie) 64-bit`, login user `debian`). `exit-node-vm/oci` also has `extra_udp_ingress_ports`. `examples/private-repo` has a bridge module, inventory group, `host_vars` and play. With the defaults, the plan and the user data of existing nodes do not change. `BRIDGE=1 make test-ansible-os IMAGE=debian13` checks a bridge with no exit routes.
- node-control `aws` provider: with no `region` in the node block and no `AWS_REGION`, the host's own region from IMDSv2 is the default (#220). It is looked up once, with a 2 s limit, so a host off EC2 is not slowed down. The error for a node with no region now names all three ways. `AWS_REGION` is in `examples/compose/control/node-control.env.example`.
- `mk/site.mk` and `scripts/deploy-fly.sh` (#194). A site repo includes `vendor/headlamp/mk/site.mk` and sets only its values (`FLY_APPS`, `NODE_CONTROL_URL`, paths). Targets: `deploy-<app>`, `plan-deploy-<app>`, `deploy-all`, `tf-plan`/`tf-apply`/`tf-save`, `ansible-site`/`ansible-check`, `bump-tailscale-version` (now also deploys the Fly exit node), `policy-*`, `tasks-*`, `rotate-api-key-check`, `submodule-check`, `verify`. `deploy-fly.sh` runs from any directory of the site repo, refuses a dirty submodule or a checkout that differs from the pin, and passes `VERSION` and `TSVERSION` from the pins to the apps that declare them. `make test-site-mk` tests it.
- `make deploy-headscale` (`scripts/deploy-fly.sh`) takes a Fly volume snapshot first, waits until it is created and prints its ID (#197). When the snapshot fails, it does not deploy. `NO_SNAPSHOT=1` skips it, and `SNAPSHOT_APPS` lists the apps that get one (default `headscale`). `scripts/fly-snapshot.sh <app> [volume]` does the same alone. `make test-fly-snapshot` tests it.
- `scripts/fleet-run.sh` (#199), used by `make ansible-site` and `make bump-tailscale-version`: it probes the nodes first (`PROBE_TIMEOUT`, default 15 s) and runs the playbook only on the nodes that answer. A stopped on-demand node or a node with no cloud capacity no longer blocks the run. It ends with `done: ...` and `pending: <node> (reason)`, and writes the pending nodes to `.fleet-pending`. `EXCLUDE=n1,n2` skips nodes, and `ONLY_PENDING=1` retries the pending ones. Exit 2 means some nodes are pending. `make test-fleet-run` tests it.
- `exit-node-vm/aws`: `authkey_from_secret` (bool). Set `true` when the auth key secret is made in the same stack (#222). Its ARN is then unknown at plan time, and the plan failed with "Invalid count argument". The default follows `authkey_secret_arn`, so existing callers do not change.
- `exit-node-vm/aws`: `state_parameter_name` keeps the tailscaled state in SSM Parameter Store (#213). A node-control stop and start (a new instance, ADR 0001) is then the same headscale node, with the same id, name and IPs, and no stale offline node is left. The node role can read and write only that parameter. `state_kms_key_arn` sets a customer managed key. A node with a stored state does not use the auth key again. `make test-ssm-state-e2e` and `make test-ssm-state-vm-e2e` test it with headscale and LocalStack.

### Changed

- Docs: `REDIS_URL` needs a percent-encoded password (#217). Self-enrolment with OIDC (`/me`) needs at least one bridge in `ENROLL_CONFIG` (#219). In `examples/compose/control`, start node-control within 1 hour of its pre-auth key, and how to check and make a new key (#218).

### Deprecated

- `examples/private-repo/fly/deploy.sh` is now a wrapper of `scripts/deploy-fly.sh` (#194). Use `make deploy-<app>`.

### Fixed

- `examples/compose/control`: while `ENROLL_DOMAIN` or `FUNNEL_DOMAIN` is not set, the edge gives the reserved `.invalid` site a local certificate (`tls internal`), so Caddy no longer retries ACME for it without end (#216). `compose.yaml` sets `ENROLL_TLS` and an empty `FUNNEL_DNS` for that. A real name gets Let's Encrypt as before.
- node-control: a `REDIS_URL` that does not parse now gives an error that says why (a password with `/`, `?`, `#` or `@`, as base64 often has) and how to percent-encode it, still with no part of the URL (#217).
- `control-host/aws` tags the root volume in the launch request and the primary network interface just after the launch (#225). `exit-node-vm/aws` (with `tags`) also tags the network interface in the launch request. A deployer that may create resources only with the site tags can now launch. The plan of an existing host shows the new root volume and network interface tags, in place.

## 0.13.0 - 2026-11-08

### Added

- `UPGRADING.md` lists, for each release from 0.10.0, what a site repo must change (#193). `scripts/check-release.sh` refuses a tag when `UPGRADING.md` has no section for its version. The wiki publishes it as `UPGRADING`.
- Dependabot opens update PRs for the Go modules, the base and compose images, the Terraform providers and the GitHub Actions. tailscale and headscale are not in it: their pins move together.
- `control-host/aws`: variable `metadata_hop_limit` (default `1`, 1 to 64) for the IMDSv2 hop limit (#204). Set `2` so that node-control on the compose bridge network can use the instance role with its `aws` provider. The default plan does not change.
- `exit-node-vm/aws`: the node can read its auth key at boot from Secrets Manager (`authkey_secret_arn`) with its own instance role (#203). The key is then not in the Terraform state, the user data or the launch template. New optional inputs: `authkey_kms_key_arn`, `enable_ssm`, `iam_path`, `iam_permissions_boundary`, `tags` (also on the group, the instances and the volumes), `tailscale_version` (signed apt repository, held), `vpc_id`, `subnet_ids`. `ssh_public_key` and `tailscale_authkey` are now optional. Set exactly one of `tailscale_authkey` and `authkey_secret_arn`.
- Rule for releases before 1.0.0 (#200): a new feature that opens a port, changes an access rule or adds a protocol is off by default. `make check-feature-defaults` (part of `make lint`) fails on a new `<name>_enabled: true` Ansible role default that is not in `scripts/feature-defaults.allow`. See "New features before 1.0.0" in `VERSIONING.md`.
- `make release-preflight` (`scripts/release-preflight.sh`, #192): before a tag, it runs every Ansible role in check mode in a systemd container, builds every image, and builds each app with the example overlay in `examples/private-repo/fly/` and starts its `fly.toml` process commands, also those of the previous release. New example overlay `examples/private-repo/fly/headscale/fly.toml` (process `app = "serve"`).

### Changed

- `exit-node-vm/aws`: the launch template requires IMDSv2 (hop limit 1) (#203). The plan of an existing node shows a new launch template version. The running instance stays until the next start.
- `reality-ingress` takes xray, its geodata and Caddy from the official images, pinned by digest (`ghcr.io/xtls/xray-core`, `docker.io/library/caddy`), not from the GitHub release CDN (#182). The control host pulls only the needed layers with the role's `files/oci-extract.py` (Python standard library; no docker, skopeo or crane on the control host or the bridge VM) and checks the sha256 of each file. New variables `xray_image`, `xray_image_sha256`, `caddy_image`, `caddy_image_sha256` with defaults; `xray_geodata_files` has a default and its `url` is optional. `xray_version`, `xray_sha256`, `caddy_version`, `caddy_sha256` are no longer required. A site that set only the versions and archive sha256 values now gets the image pins, and those values are ignored. To keep the archive downloads (for example from a mirror), set `xray_url` with `xray_sha256` and `caddy_url` with `caddy_sha256`; the URL defaults are now empty. The control host needs access to `ghcr.io` and `registry-1.docker.io`.
- `reality-ingress` and `tailscale` support `aarch64` nodes (#178): they use the arm64 image manifest or `.deb`. `xray_image_sha256` and `caddy_image_sha256` (new in this release) are maps by architecture: `{amd64: <sha256>, arm64: <sha256>}`.

### Fixed

- All six Ansible roles pass check mode and a real apply in the OS matrix on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13 (#178). Check mode on a fresh host no longer fails in `task-runner` (owner of the state directory before the user exists) and in `tailscale` (`sysctl` before `procps` is installed). `tailscale` no longer fails when `AutoUpdate.Apply` is null on a fresh node. `make test-ansible-os` uses the native architecture of the docker engine (`ANSIBLE_OS_PLATFORM` forces one).
- The `reality-ingress` preflight error for Hysteria 2 now says that `hy2_enabled` is on by default, which UDP port it opens, and that `hy2_enabled: false` turns it off (#200).
- `reality-ingress` keeps the pulled and downloaded files in a control-host cache, `reality_ingress_cache_dir` (default `~/.cache/headlamp/reality-ingress`), keyed by sha256 (#198). Each cached file is hashed again on every run. A run with nothing to do now shows `changed=0`, and `--check` shows only the real changes on the host (it also checks the binaries and geodata on the host, which it skipped before).

## 0.12.0 - 2026-11-08

### Added

- `make test-ansible-os IMAGE=<ubuntu-minimal|ubuntu-server|debian13>` runs each Ansible role in check mode and then for real in a throwaway systemd container, with a real headscale (#170). Each role README has a "Supported systems" part.
- Each release publishes the Helm charts to `oci://ghcr.io/valentinalexeev/headlamp/charts`, with the release version (#158).
- CI runs more checks on each PR: the tailscale and headscale pins (#150), the Ansible role tests, `make lint-go` for new Go code, `make vulncheck` and `make lint-config` (#160). A weekly workflow opens an issue when a newer tailscale or headscale is out.
- Helm chart `charts/tailnet-connector` (#103): a subnet router for one Kubernetes cluster, so tailnet devices reach its Service and Pod ranges. One userspace tailscale replica joins with an ephemeral tagged key from a Secret, advertises `connector.routes`, and keeps its state in a Kubernetes Secret; its Role can touch only that Secret. headscale approves the routes through `autoApprovers.routes` for the connector tag. Optional, off by default: serve proxies (`funnel-sidecar` in serve mode, #117), one Deployment per node-control service. New image `tailnet-connector` (tailscale at `TAILSCALE_VERSION`, from the official static tarball); the chart `appVersion` is in `make check-tailscale-pins`. `make test-k8s-connector` runs helm lint and kubeconform; `make test-k8s-connector-e2e` runs it in kind against the pinned headscale.
- node-control has an Access page (`/admin`) to change roles and grants in the console (#123). It is on when `ADMIN_DB` (SQLite) or `REDIS_URL` is set. Grants from `ADMIN_USERS`, `ADMIN_CONFIG` and the old lists still apply and are locked on the page, so they work as break-glass owners. Nobody can change their own grants or give more than they hold. Every change goes to a change log. Import and export use the `ADMIN_CONFIG` format. A new permission feature `admin` gates the page; only `owner` has it, and `*.level` does not include it.
- Tailnet grants through a bridge, phase 2 (#139). A bridge with `"tailnet_access": "grants"` gives each device only the tailnet targets it holds. Targets are named sets of ACL destinations in `enroll.json` (`tailnet_targets`) or in the console. A grant goes to a device or to an owner (all their devices, also later ones); a `group:<name>` grant is stored but "not resolved" until OIDC groups are known (#102). Devices with the same targets form a grant group with its own tailnet identity on the bridge (`tag:ingress-g-<8 hex>`), so the headscale ACL is the real limit; at most 8 groups per bridge. A group has no tailnet route until the bridge has its SOCKS instance (deny by default). node-control generates one ACL rule and one `tagOwners` entry per group; `GET /enroll/tailnet-acl` shows the diff and `POST` applies it as one confirm-or-revert change. The bridge page has the mode selector (`off`, `all`, `grants`), the targets, the group status and the ACL preview; the Devices page has a Tailnet section with check boxes for device and owner grants. Changes need `enroll.admin` and go to the audit log. "Convert to grants" moves a bridge from `all` without a change in access. To provision a group: add it with its tag key to the new `socks_grant_groups` of the `tailscale-userspace-socks` and `reality-ingress` roles and run the playbook. The ingress agent accepts a group id as `tailnet` and routes it to `via-tailnet-<id>`. `make test-tailnet-access-e2e` also runs the grants matrix over VLESS and Hysteria 2.

### Deprecated

- `"tailnet_access": "all"` (#139). It keeps working in 0.x. Use `"grants"`, or "Convert to grants" on the bridge page.

### Changed

- CI: the gitleaks secret scan runs the official `ghcr.io/gitleaks/gitleaks` image, pinned by digest, and the Hysteria 2 e2e bridge takes xray from the official `ghcr.io/xtls/xray-core` image. Before, both downloaded release files from the GitHub release CDN, which gave HTTP 504 for hours and failed CI.
- `control-host/aws` uses Ubuntu 26.04 Minimal by default (was Ubuntu 26.04 Server) (#171). New input `image_flavor`, default `minimal`. Set `server` to keep the old image. New input `ami_id` sets any other AMI. The instance ignores AMI changes, so a running host is not replaced.
- CI runs on `ubuntu-26.04` runners.
- The exit-node VM modules use Ubuntu 26.04 "resolute" by default where the cloud publishes an image (#164). Running VMs are not replaced. Each module keeps an input to pin the old image:
  - `aws`: new input `ubuntu_series`, default `resolute-26.04` (was a fixed `noble-24.04`). New input `image_flavor`, default `minimal`: the module uses the Canonical Ubuntu Minimal AMI (fewer packages and services, no snapd). New input `ami_id` sets any other AMI. To keep the old image, set `ubuntu_series = "noble-24.04"` and `image_flavor = "server"`.
  - `openstack` and `selectel`: `image_name` default is `Ubuntu 26.04 LTS 64-bit` (was `Ubuntu 22.04 LTS 64-bit`). To keep the old image, set `image_name = "Ubuntu 22.04 LTS 64-bit"`.
  - `yandex`: new input `image_family`, default `ubuntu-2604-lts`. `image_id` is now optional and wins when set, so a stack that sets it keeps its image. The VM now ignores image changes, so a newer image does not replace it.
  - `oci`: `ubuntu_version` default is `26.04 Minimal` (was `24.04`). New input `ubuntu_version_fallback`, default `24.04 Minimal`, is used when OCI has no image of `ubuntu_version`. To keep the old image, set `ubuntu_version = "24.04"`.
  - `cloudru`: unchanged at `image_name = "ubuntu-22.04"`, because no public source confirms a 26.04 image in Cloud.ru Evolution.

### Fixed

- Ansible roles on minimal images (#170): `route-provisioner` installs `cron` (Ubuntu Minimal and Debian 13 have no `crontab`); `tailscale` installs `procps` for `sysctl`, starts `tailscaled` after the install, and its check mode no longer fails on a host without tailscale.
- `make vulncheck` pins govulncheck v1.9.0. v1.1.4 stopped with a panic on Go 1.27 code (`unexpected expr: *ast.KeyValueExpr`), so the check failed on a clean machine.
- `apps/exit-node/install.sh` rejected Ubuntu noble and resolute, because it had a fixed list of codenames (#164). It is now synced with the upstream Tailscale script, which asks the package server instead.

## 0.11.0 - 2026-11-07

### Added

- `terraform/modules/control-host/aws` has two optional inputs, `iam_path` and `iam_permissions_boundary` (#148). They let the module run in accounts that allow new roles only under a fixed path and with a boundary. The defaults keep the plan of an existing host unchanged.
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

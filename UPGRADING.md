# Upgrading a site

This file is for a site repo that vendors headlamp as a git submodule (see `examples/private-repo/`). Each section
lists only what the site must do, or check, when it moves to that release. The full list of changes is in
[CHANGELOG.md](CHANGELOG.md). The newest release is first.

To upgrade more than one release, do the sections in order, from the oldest to the newest.

Each release has a section here before its tag (see [VERSIONING.md](VERSIONING.md)). A release with no site action
says "No action required".

## Unreleased (0.14.0)

### Action required

No action required.

- **Recommended for AWS exit nodes** (#213): set `state_parameter_name` (for example `/headlamp/<node>/tailscaled`)
  and `tailscale_version` on each `exit-node-vm/aws` module. Then a node-control stop and start keeps the same node.
  The deploying identity needs the IAM actions of `authkey_secret_arn` (role, profile, `iam:PassRole`). The first
  start after the change registers once more: delete the old node in headscale after it
  (`headscale nodes delete -i <old id>`).

  Check: after a stop and start, `headscale nodes list` shows the same id and name, and no `<name>-1`.


### Action required

- **Terraform providers: AWS 5 to 6, OCI 6 to 9** (#186, #187, #188). The modules `control-host/aws` and
  `exit-node-vm/aws` now need `hashicorp/aws ~> 6.72` (was `~> 5.0`). The module `exit-node-vm/oci` now needs
  `oracle/oci ~> 9.12` (was `~> 6.0`). A root stack that pins the old major cannot init.
  1. In the root `versions.tf`, set `aws = { source = "hashicorp/aws", version = "~> 6.72" }` and
     `oci = { source = "oracle/oci", version = "~> 9.12" }`.
  2. Run `terraform init -upgrade`. This updates `.terraform.lock.hcl`. Commit the lock file.
  3. Run `terraform plan` and read it before you apply. Expect no replacement of an instance. If the plan replaces a
     resource, stop and open an issue.

  Check: `grep -rn 'hashicorp/aws\|oracle/oci' --include='*.tf' .` shows only `~> 6.72` and `~> 9.12`.
- **`reality-ingress`: xray and Caddy come from pinned images** (#182, #185). The role now takes xray, its geodata
  and Caddy from `ghcr.io/xtls/xray-core` and `docker.io/library/caddy`, pinned by digest (`xray_image`,
  `xray_image_sha256`, `caddy_image`, `caddy_image_sha256`, with defaults). The site variables `xray_version`,
  `xray_sha256`, `caddy_version` and `caddy_sha256` are no longer required, and the role ignores them unless the
  matching URL is set.
  - Do nothing to use the image pins. Remove the old version and sha256 values from the site vars to keep them clean.
  - To keep the archive downloads (for example from a mirror), set `xray_url` with `xray_sha256` and `caddy_url` with
    `caddy_sha256`. The URL defaults are now empty.
  - The control host must reach `ghcr.io` and `registry-1.docker.io` over HTTPS.

  Check: `grep -rn 'xray_version\|xray_sha256\|caddy_version\|caddy_sha256\|xray_url\|caddy_url' ansible/`.
- **`exit-node-vm/aws`: an existing node gets a new launch template version** (#203). The launch template now
  requires IMDSv2 (hop limit 1). `terraform plan` shows `metadata_options` on the launch template and nothing else for a
  caller that sets `tailscale_authkey` and `ssh_public_key`. The user data does not change. The running instance stays.
  The group uses the new version at the next start, for example after a stop and start by node-control.
  - Software on the node that reads instance metadata with IMDSv1 (no token) stops working. headlamp itself does not
    use IMDSv1.
  - Recommended, not required: move the auth key out of the state. Put the key in Secrets Manager, set
    `authkey_secret_arn`, and remove `tailscale_authkey`. The module then makes a node role (`iam_path`,
    `iam_permissions_boundary` if the account needs them) that can read only that secret. The deploying identity
    needs `iam:CreateRole`, `iam:PutRolePolicy`, `iam:CreateInstanceProfile`, `iam:AddRoleToInstanceProfile` and
    `iam:PassRole` for it.
  - `ssh_public_key` is now optional. If you remove it, the plan destroys the key pair. Nothing uses it.

  Check: `terraform plan` on the exit-node stack shows only `metadata_options` changes on `aws_launch_template`.

### Defaults that changed

- The exit-node image is built on `alpine:3.20` (was `alpine:3.18`) (#189). A site overlay for `exit-node` that adds
  packages with `apk` must work on Alpine 3.20. Check: `fly/deploy.sh exit-node` builds with no error.
- `control-host/aws` has `metadata_hop_limit` (default `1`, as before) (#204). Set `2` only if node-control on the
  compose bridge network must use the instance role with its `aws` provider. Then every container on the host can use
  the role. The default plan does not change.

## 0.12.0

### Action required

- **headscale: stock image, new entrypoint, no route approver** (#152, #153). The image is now
  `ghcr.io/juanfont/headscale` v0.29.7 (distroless, no shell). Its entrypoint is the headscale binary, so the process
  command is only its arguments. The approver scripts (`approve-gateway-routes.sh`, `cron-approve-routes.sh`,
  `init.sh`) are removed. This was not in the 0.12.0 changelog.
  1. In the site overlay `fly/headscale/fly.toml`, set `[processes] app = "serve"` (was `app = "headscale serve"`).
     Remove an approver process if you added one.
  2. Approve routes with `autoApprovers.routes` in the site `acl.json`. Give each `tag:gateway` prefix, with the IPv4
     and the IPv6 half. See `apps/headscale/acl.json`.
  3. The `HEADSCALE_API_KEY` secret of the headscale app is no longer used. You can remove it.
  4. Use `flyctl ssh console -C "headscale ..."` or `docker exec <ctr> headscale ...`. There is no `sh` in the image.
  5. In compose, set `command: serve`. The healthcheck is `headscale health`.

  Check: `grep -n '^app' fly/headscale/fly.toml` shows `app = "serve"`, and
  `grep -n autoApprovers fly/headscale/acl.json` finds the block.
- **Bridge grants, new role variable** (#139, #180). To use `"tailnet_access": "grants"`, add each grant group with
  its tag key to `socks_grant_groups` of the `tailscale-userspace-socks` and the `reality-ingress` roles, and run the
  playbook. The default is `[]`, so a site that does not use grants has no action.

### Defaults that changed

- `control-host/aws` uses Ubuntu 26.04 Minimal (#171). Set `image_flavor = "server"` to keep the old image. A running
  host is not replaced.
- The exit-node VM modules use Ubuntu 26.04 where the cloud has it (#164). Running VMs are not replaced. To keep the
  old image on a new VM:
  - `aws`: `ubuntu_series = "noble-24.04"` and `image_flavor = "server"`.
  - `openstack`, `selectel`: `image_name = "Ubuntu 22.04 LTS 64-bit"`.
  - `oci`: `ubuntu_version = "24.04"`.
  - `yandex`: set `image_id` (it wins over the new `image_family`).

### Deprecated

- `"tailnet_access": "all"` in `enroll.json`. Use `"grants"`, or "Convert to grants" on the bridge page.

## 0.11.0

### Action required

- **Build arg `VERSION` for node-control.** The console shows the headlamp version from the `VERSION` build arg.
  Without it, the console shows `dev`. Pass it when the site builds the image:
  `fly/deploy.sh node-control --build-arg VERSION=$(cat vendor/headlamp/VERSION)`. Published images have it.
- **Tailscale pin 1.104.5** (#145). Set the site `TAILSCALE_VERSION` file to `1.104.5` and run
  `make bump-tailscale-version VERSION=1.104.5`. The Ansible `tailscale` role now holds the package and turns off
  self-update (`tailscale_pin_version`, default `true`).
- **`ingress-agent` role fix** (#144). In 0.10.0 the role preflight failed on every bridge. If you stayed on an old
  agent because of this, run the playbook for the bridges again.

## 0.10.0

### Action required

- **Paths moved** (#119). `apps/fly/<x>` is now `apps/<x>`, and `apps/fly/exit-ams` is now `apps/exit-node`.
  1. Replace `vendor/headlamp/apps/fly/` with `vendor/headlamp/apps/` in every script and CI file.
  2. Rename the overlay directory `fly/exit-ams/` to `fly/exit-node/`. Deploy with `fly/deploy.sh exit-node`.
  3. Go imports: `github.com/valentinalexeev/headlamp/apps/<component>`. Image names do not change.

  Check: `grep -rn 'apps/fly/\|exit-ams' --exclude-dir=vendor .` finds nothing.
- **Console pages need a role** (#122). With an admin configured, every console page needs a permission. Before,
  the overview, nodes and enrolment pages had no user check. The old lists `POLICY_ADMIN_USERS`,
  `FUNNEL_ADMIN_USERS` and `TASKS_ADMIN_USERS` give admin of their feature only, so their users lose the other
  pages. Set `ADMIN_USERS` to the operator logins (role `owner`), or give roles in `ADMIN_CONFIG`.
  Check: sign in as the operator and open Overview, Nodes and Devices.
- **Hysteria 2 is on by default** (#110). The `reality-ingress` role has `hy2_enabled: true`, and its preflight
  fails without `hy2_obfs_password` (16 to 128 characters of `[A-Za-z0-9_-]`). Do one of these:
  - Set `hy2_obfs_password` in the vault (`openssl rand -hex 16`), open the UDP port, and set `hy2_port` in
    `enroll.json` and `HY2_OBFS_<NAME>` for node-control. A bridge that adds `hy2_port` needs a new approval.
  - Set `hy2_enabled: false` to keep VLESS only.
- **`ingress-agent` is now Go** (#133). The role builds it on the control host, so the control host needs Go 1.26 or
  later. Note: the 0.10.0 role preflight is broken (#144). Go to 0.11.0 or later.
- **`exit-node-vm/aws` tag permission.** The role that applies the module needs `autoscaling:CreateOrUpdateTags`.

### Defaults that changed

- node-control: `FUNNEL_DNS=ondemand` is the new default (#127). Fly.io keeps the manual checklist.

### Deprecated

- The node-control provider `selectel` and the module `exit-node-vm/selectel`. Use `openstack` and
  `exit-node-vm/openstack`. The wrapper has `moved` blocks, so the plan shows no change.

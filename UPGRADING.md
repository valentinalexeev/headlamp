# Upgrading a site

This file is for a site repo that vendors headlamp as a git submodule (see `examples/private-repo/`). Each section
lists only what the site must do, or check, when it moves to that release. The full list of changes is in
[CHANGELOG.md](CHANGELOG.md). The newest release is first.

To upgrade more than one release, do the sections in order, from the oldest to the newest.

Each release has a section here before its tag (see [VERSIONING.md](VERSIONING.md)). A release with no site action
says "No action required".

## 0.19.0

### Optional

- **Tailnet DNS** (#285): the module now has zones, `CNAME`, `TXT` and `SRV` records, and rules per user or tag.
  The records of 0.18 stay `A` records and keep their answers, and the extra-records file does not change. No action
  is required. The extra-records file does not contain a zone, a record with rules, or a record of another type. When
  you add one of them, add its new line of `GET /dns/state` `split` (or of the DNS page) to
  `dns.nameservers.split` in `headscale.yaml`. Then restart headscale.

- **Serve certificates from an ACME CA** (#334): no action required. `SERVE_CERT_SOURCE` defaults to `ca`, and the
  chart value `serve.certificate.source` defaults to `node-control`, so serve works as before. To opt in, see
  "Certificates from an ACME CA" in the node-control README, or "Serve certificates" in the tailnet-connector chart
  README. With `SERVE_CERT_SOURCE=acme`, upgrade every `funnel-sidecar` in serve mode to 0.19.0 first: node-control
  answers `426` to an older one.

## 0.18.1

### Action required

- **Only if you set `COMPAT_STRICT=true`** (#322): node-control now also refuses a component that reports no interface
  version, that is, a component older than 0.18.0. Before you deploy node-control 0.18.1, run `scripts/fleet-check.sh`
  (or open the Fleet page) and upgrade each component that shows "reports no ... version". Else that ingress agent
  gets no credentials, the funnel ingress gets no routes, and that task runner gets no task.

  Check: after the deploy, `fleet-check.sh` exits 0, and Attention has no "is refused (COMPAT_STRICT)" item.

  Without `COMPAT_STRICT`, no action is required.

## 0.18.0

### Action required

- **Each front that serves `link_host`** (#306): copy the new `enroll-paths.caddy` (`examples/compose/control`:
  `caddy/enroll-paths.caddy`, then `docker compose up -d edge`), or run the `reality-ingress` role on each bridge. It
  allows `/s/`, the stylesheet and fonts of the self-service pages. Until then, these pages show without the console
  style. They still work.

  Check: open `https://<link_host>/s/style.css`. It must return the stylesheet, not 404.

### Optional

- **Compatibility matrix** (#300): node-control now shows on the Fleet page, in Attention and in `fleet-check.sh`
  each component whose interface version it does not accept. A component of an older release reports no version
  and shows a note, not a problem. To make node-control refuse an incompatible component, set `COMPAT_STRICT=true`.
  Do this only after every component runs 0.18.0 or newer. See "Compatibility" under "Fleet" in the node-control
  README.
- **ingress-agent role** (#300): the role now builds the agent with the version of the last commit that changed the
  agent. A release that did not change the agent does not restart it.
- **Secrets in compose env files** (#302): put each secret value with a `$` in single quotes. Compose expands `$` in
  a value without them. After the upgrade, compare the `credential fingerprint` lines in the node-control start log
  with your values (see the compose README).
- **Tailnet names through bridges** (#286): to let bridge clients with a tailnet grant reach tailnet services by name,
  do these steps. See "Tailnet DNS module" in the node-control README.
  1. Set `TAILNET_DNS_DB` on node-control, for example `/var/lib/tailscale/dns.db`. Or set `TAILNET_DNS_REDIS=1`
     with `REDIS_URL`.
  2. Grant `dns.admin` to an operator in the admin policy.
  3. Add a policy rule that lets the bridge tag reach node-control on TCP port 53.
  4. Run the `ingress-agent` role on each bridge, so that the agent accepts the resolver.
  5. Deploy node-control.

  Check: `GET /dns/state` shows `listening: true` and a `bridge` entry. After the next sync, a granted client opens a
  tailnet service by its name.

  Without `TAILNET_DNS_DB` or `TAILNET_DNS_REDIS=1`, nothing changes.
- **Tailnet DNS records for all devices** (#286), compose stack only: node-control can write its records into the file
  of headscale's `dns.extra_records_path`. Do the steps of "Tailnet DNS (optional)" in
  `examples/compose/control/README.md`. The new `compose.yaml` mounts `${DATA_DIR}/dns` into headscale and
  node-control. The mount alone changes nothing.

  **CAUTION:** headscale does not start when `dns.extra_records_path` names a file that does not exist. Create the
  file with `[]` before you set it.
- **Self-service Extend** (#301): to let people extend their own devices on `/me`, set `OIDC_EXTEND_WINDOW_DAYS`
  (and optionally `OIDC_EXTEND_GRACE_DAYS`) on node-control. The new `enroll-paths.caddy` of the action above also
  allows the Extend button. Without the variable, nothing changes.
- **Bridge selection** (#313): to tune how device profiles choose between bridges, add the optional `urltest` object
  and a `group` on bridges to `enroll.json`. See "Bridge selection in device profiles" in the node-control README.
  Without them, the profiles do not change.

## 0.17.0

No action required. Deploy the new node-control image.

### Optional

- **Throne** (#295): the app picker now offers Throne on Android, Windows, macOS and Linux. If your `enroll.json` has
  a `clients` list, add `{"id": "throne"}` to offer it. Throne for Android is not on Google Play: give its download
  link in `install.android`, for example the GitHub release page of Throne for Android.
- **Devices page** (#287): the People and Invitations tables are now one tree with the devices. Tell your operators.
  Nothing in `enroll.json` or the APIs changed.

## 0.16.1

No action required. Deploy the new node-control and funnel-ingress images (Go 1.27.4). The fix of #288 also
removes the need for the manual `hs_user_id` reset after you delete a headscale user of a person.

## 0.16.0

### Action required

- **Each front that serves `link_host`** (#271): copy the new `enroll-paths.caddy` (`examples/compose/control`:
  `caddy/enroll-paths.caddy`, then `docker compose up -d edge`), or run the `reality-ingress` role on each bridge. It
  has the app page `/me/devices/<id>/app/<app>`. Until then, the app picker buttons on an iPhone answer 404. **Open without the check** under each button still works.

  Check: on an iPhone, open the app picker of a device in Safari, select an app, and make sure that the app page
  opens.

### Optional

- **Health checks** (#196): run the `ingress-agent` role on each bridge for the bridge self-test (on by default; set
  `ingress_egress_socks: ""` to turn it off). Take the new `fly.toml` of the exit node and node-control for their Fly
  checks: `fly checks list -a <app>` shows them after the deploy. After an upgrade, run
  `NODE_CONTROL_URL=http://<node-control> scripts/smoke-test.sh` from a tailnet node of an operator.
- **Fleet versions** (#195): run the `ingress-agent` role on each bridge and deploy the new funnel-ingress image, so
  that they report their versions. Then `make fleet-check` (or `make verify`) in the site repo, or the Fleet page of
  node-control, shows the version and state of every part. Its login needs the Overview view permission. A bridge
  agent or funnel ingress before 0.16.0 shows "unknown" and is not a failure.

## 0.15.3

No action required. The fix of #270 is in node-control only: deploy the new image.

## 0.15.2

### Action required

- **Control host with `ENROLL_DOMAIN`** (`examples/compose/control`, #260): copy the new `caddy/Caddyfile` and
  `caddy/enroll-paths.caddy`, and the new `compose.yaml` (it mounts the second file). Then run
  `docker compose up -d edge`. If you changed your Caddyfile, replace its `@public` list with `import
  enroll-paths.caddy` and the 2 `handle` blocks of the example. Check: `curl -s -o /dev/null -w '%{http_code}'
  https://<ENROLL_DOMAIN>/me` gives `200`.
- **Bridges** (`reality-ingress` role): run the role. It now writes `enroll-paths.caddy` next to the Caddyfile. The
  front serves the same paths as before.
- **Bridges** (`ingress-agent` role): run the role on each bridge. The new agent sends the DNS lookups of xray
  through the exit node. Check: `jq '.dns, .routing.rules[0]' /etc/xray/config.json` shows tag `dns-internal` and a
  rule that sends it to `via-exit`. To use other resolvers, set `ingress_exit_dns` (IP addresses only).
- **headscale policy, sites with bridges** (#259): make sure that the policy has the bridge rules of the example
  `examples/compose/control/config/acl.json`, mainly `tag:ingress` to `autogroup:internet:*`. The
  `tailscale-userspace-socks` role now fails when traffic does not get through the exit node. Add the rules before
  you run it, or set `socks_egress_check: false` for that run.

## 0.15.1

### Optional

- **Control host serves `link_host`** (`examples/compose/control` with `ENROLL_DOMAIN`): take the new `compose.yaml`
  and run `docker compose up -d`. It sets `ENROLL_PUBLIC_LISTEN=:8081` for node-control, so the edge reaches the
  enrolment pages. Check: `curl -s -o /dev/null -w '%{http_code}' https://<ENROLL_DOMAIN>/me` gives `200`.
- Other sites: no action required.

## 0.15.0

### Action required

- **Bridges that serve the link host** (#241): run the `reality-ingress` role on each bridge, for example with
  `make ansible-site`. The new front also proxies `/sub/<secret>/<format>`, the links of the app picker. With
  `enroll_self_service: true`, it also proxies `/me/devices/<id>`, `/me/icons/<app>` and `/i/<token>`. Until then, an
  old front answers 404 to these links. The links of earlier enrolments do not change.

  Check: for the subscription URL of an active test device,
  `curl -s -o /dev/null -w '%{http_code}' https://<link_host>/sub/<secret>/uri` gives `200`.
- **Your own edge in front of node-control** (not the role, not `examples/compose/control`): also proxy `/i/*` to
  `:8081` when you use self-enrolment with OIDC.

### Optional

- Add `clients` to `enroll.json` to choose the apps of the app picker and their store links. Put the app icons in a
  directory of the site repo and set `ENROLL_CLIENT_ICON_DIR`. See the node-control README, "Client apps and profile
  formats" and "App picker".
- Set `OIDC_INVITE_ONLY=true` to allow new devices only with an invitation.
- OpenStack nodes with `bootstrap_tailscale = true`: set `authkey_from_secret = true` to keep the auth key out of user
  data (#224). Or use a single-use key that expires after 1 h. See the `exit-node-vm/openstack` README.

## 0.14.0

### Action required

No action required. The old site files and modules keep working.

- **Recommended: stop copying the site scripts** (#194). Replace the targets of your site `Makefile` with
  `include vendor/headlamp/mk/site.mk` and your values, as in `examples/private-repo/Makefile`. Replace your copy of
  `fly/deploy.sh` with the wrapper in `examples/private-repo/fly/deploy.sh`, or delete it and use `make deploy-<app>`.
  Then a release that moves a path or adds a build arg needs no change in your site repo.

  Check: `make help` lists the headlamp targets, and `make verify` passes.
- **Recommended for AWS exit nodes** (#213): set `state_parameter_name` (for example `/headlamp/<node>/tailscaled`)
  and `tailscale_version` on each `exit-node-vm/aws` module. Then a node-control stop and start keeps the same node.
  The deploying identity needs the IAM actions of `authkey_secret_arn` (role, profile, `iam:PassRole`). The first
  start after the change registers once more: delete the old node in headscale after it
  (`headscale nodes delete -i <old id>`).

  Check: after a stop and start, `headscale nodes list` shows the same id and name, and no `<name>-1`.
- **AWS: a secret made in the same stack** (#222). If your root stack makes the auth key secret in the same apply as
  `exit-node-vm/aws`, set `authkey_from_secret = true`. Without it, the plan fails with "Invalid count argument".

  Check: `terraform plan` of the stack has no "Invalid count argument".

- **Bridge on OpenStack or Selectel** (#235): no action for existing exit nodes. For a new bridge, see the
  `exit-node-vm/openstack` README, "Bridge node". The `selectel` wrapper passes the new inputs through.

### Defaults that changed

- `control-host/aws` and `exit-node-vm/aws` (with `tags`) now tag the root volume and the network interface (#225).
  `terraform plan` shows these tag changes in place. A deployer policy with `aws:RequestTag` conditions: see the
  control-host README, "Deploying identity with tag conditions".

### Defaults that changed

- `make ansible-site` and `make bump-tailscale-version` (from `mk/site.mk`) skip the nodes that do not answer within
  `PROBE_TIMEOUT` seconds (default 15), and list them as pending (#199). `make ansible-site` exits 2 when a node is
  pending. Add `.fleet-pending` to your `.gitignore`.
- `make deploy-headscale` now takes a volume snapshot before the deploy, and stops when the snapshot fails (#197). The
  deploying Fly token needs to create snapshots. For a deploy with no snapshot, set `NO_SNAPSHOT=1`.


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

# task-runner

Deploys the generic task runner (`apps/task-runner/` in this repo, see its README for the API) on a VM node as
`task-runner.service`: an unprivileged service user, rootless podman, the run image (Chromium + Playwright + the
runner's `host.js`), the env file with the token, and an nftables guard that keeps the service user (and so every
container it starts) off loopback, the LAN and the tailnet. node-control calls the runner over the tailnet.

Not verified against a real host yet: see [Not verified](#not-verified-against-a-real-host) before the first deploy.

## Use

There is no playbook in this repo (like the other node roles); the private repo applies the role to an inventory group,
after the base `tailscale` role:

```yaml
- name: Task runners
  hosts: task_runners
  roles:
    - role: task-runner
      tags: [task-runner]
```

Site values (from host_vars / the vault in the private repo, never in this repo):

```yaml
task_runner_src: "{{ playbook_dir }}/../vendor/headlamp/apps/task-runner"
task_runner_token: "{{ vault_task_runner_token }}"          # openssl rand -hex 32
task_runner_listen_addr: "<tailnet-ip>"                       # `tailscale ip -4` on the node
task_runner_base_image: "<registry>/<repository>@sha256:<64 hex>"
```

Give the node the runner tag in headscale (node-control's `TASKS_RUNNER_TAG`, default `tag:task-runner`): node-control
never contacts an untagged node. Allow the runner port (`task_runner_port`, default 9100) to that tag from node-control
only, in the tailnet ACL. Tag the node as an admin: run `scripts/headscale-tag-node.sh show --node <given-name>` first
and read the tag model it prints (`unified` on headscale 0.28+, one `tags` list; `legacy` before, forced and valid
tags), then `add --node <given-name> --tag tag:task-runner` without `--yes` to see the plan, then with `--yes`, and
`show` again to verify. On the unified model, tagging a node that has no tags removes its user ownership (undone only
by re-authenticating with empty tags), so the helper also asks for `--allow-ownership-change`. `headscale nodes tag` or
a pre-auth key with that ACL tag work too. The policy's `tagOwners` decides who may have the tag at registration (and,
on legacy servers, who may request it later), so that entry is the trust boundary: list only the admin/service
identity as owner of the runner tag, never human users whose devices are not runners.

**systemd-resolved hosts:** if `/etc/resolv.conf` points at the local stub (`nameserver 127.0.0.53`), add
`127.0.0.53` to `task_runner_guard_resolvers` (keep the tailnet entries if they are used). Preflight fails safe when a
blocked-range resolver is missing, but the play stops there until the list is right.

**Deploy when the runner is idle.** A rerun that changes the binary, the unit or the env file restarts the service,
which cancels an active run (it is reported as cancelled to node-control). Check `GET /v1/health` (`busy`) first.

Tags: `task-runner` (everything), `install`, `guard`, `image`, `config`, `image-prune` (only the pruning of old image
tags). Preflight is tagged `always`, so it runs under every `--tags` and is not dropped by `--skip-tags` of any other
tag (only `--skip-tags always` removes it). `--tags install` alone installs the binary and unit and notifies the restart
before `config` writes the env file: on a fresh node run the whole role (no tags) the first time.

Check mode on the node is safe: files are templated, no image is built or loaded and nothing is started or reloaded.
The Go binary IS built on the control host (into a temp dir, `check_mode: false`) so the copy can be compared; nothing
reaches the node. On a fresh host the steps that need the service user may fail in check mode (as in the other roles).

## Variables

| Variable | Default | Notes |
|---|---|---|
| `task_runner_src` | `""` | Required. Path on the **control host** to `apps/task-runner`. |
| `task_runner_token` | `""` | Required, vault. At least 24 characters, no whitespace, quotes, backslash, `$` or backtick. Never logged. |
| `task_runner_listen_addr` | `""` | Required. The node's tailnet address (100.64.0.0/10, or IPv6 in fd7a:115c:a1e0::/48). Preflight checks `tailscale ip` on the node reports it. |
| `task_runner_port` | `9100` | Bound on `task_runner_listen_addr` only. |
| `task_runner_session_enabled` | `false` | Turns on interactive browser sessions: adds `RUNNER_SESSION_BIND` (= `task_runner_listen_addr`) and `RUNNER_SESSION_PORT` to the env file. See [Browser sessions](#browser-sessions). |
| `task_runner_session_port` | `6080` | noVNC port published on the tailnet address; used only when sessions are enabled. 1-65535, must differ from `task_runner_port` (preflight). |
| `task_runner_base_image` | `""` | Required unless `task_runner_image_archive` is set. `<image>@sha256:<64 hex>`, see [Base image digest](#base-image-digest). |
| `task_runner_image_archive` | `""` | Optional control-host path to a `podman save` tarball; loaded instead of building. |
| `task_runner_binary` | `""` | Optional control-host path to a prebuilt linux binary; otherwise built with the control host's `go`. |
| `task_runner_memory_max` | `2G` | systemd `MemoryMax` for the whole service (runner, podman, the container). |
| `task_runner_container_memory` | `1g` | `RUNNER_MEMORY` (podman `--memory`); at least 256 MB below `task_runner_memory_max`. |
| `task_runner_pids_limit` / `task_runner_tasks_max` | `512` / `2048` | Per container (`RUNNER_PIDS_LIMIT`) / whole service (`TasksMax`). |
| `task_runner_min_free_mb` / `task_runner_min_free_disk_mb` | `1500` / `6000` | Free memory without page cache (preflight); free disk on the file system of the state dir (checked in the image step, only when an image has to be built or loaded). |
| `task_runner_guard_resolvers` | `["100.100.100.100", "fd7a:115c:a1e0::53"]` | Resolvers the service user may query on port 53 although they are in a blocked range. |
| `task_runner_guard_extra_drop` | `[]` | IPv4/IPv6 addresses or CIDRs the service user may never reach, dropped after the DNS accepts. List the node's public address here when it is not configured on an interface (1:1 NAT, see Guard). Entries must not overlap (preflight checks IPv4 ranges and IPv6 duplicates). |
| `task_runner_guard_check_interval` | `30s` | How often the guard check timer looks for the table. |
| `task_runner_protect_kernel_tunables` | `false` | systemd `ProtectKernelTunables=`; off because it very likely breaks every run (see Service hardening). |
| `task_runner_image_async` / `task_runner_image_poll` | `3600` / `30` | Seconds allowed for a build or load, and the poll interval. |

Fixed layout (variables exist, change only with reason): user `task-runner`, state dir and HOME `/var/lib/task-runner`
(0700), runtime dir `/run/task-runner` (0700), env file `/etc/task-runner/env` (0640 `root:task-runner`), binary
`/usr/local/bin/task-runner`, guard file `/etc/task-runner-guard.nft`, image repository `localhost/task-runner`.

## What it does

1. **Preflight** (read-only): facts, Debian family with apt, x86_64 or aarch64, required variables, token shape, listen
   address in the tailnet ranges and present on the node, limits, resolvers and `task_runner_guard_extra_drop`, base image digest, the source on the
   control host, Go (unless `task_runner_binary`), free memory and disk, every blocked-range resolver of
   `/etc/resolv.conf` listed in `task_runner_guard_resolvers`. Free memory fails the play only when the service is not
   running yet (otherwise a warning); free disk is checked in the image step, only when the image has to be built or
   loaded. Warns (never fails) when the inventory address (`ansible_host`) is an IP that is not on any interface of the
   node and not in `task_runner_guard_extra_drop` (a likely 1:1-NAT public address the guard would not cover). That
   warning only sees `ansible_host`: when the inventory reaches nodes by their tailnet address (the usual case here) it
   never fires, so the `ip -o addr` check in "Rollout checks" (step 3) is the real control for a NAT'd public address.
   Fails when `task_runner_guard_extra_drop` has an octet above 255 or overlapping/duplicate entries (nft refuses
   overlapping intervals in one set; IPv4 entries are compared as ranges, IPv6 entries only for exact duplicates).
   Works out the image tag.
2. **Install**: `podman`, `uidmap`, `passt`, `acl` (Ansible needs `setfacl` to run commands as the service user); the
   package's root-level units (`podman.socket`, `podman-restart.service`, `podman-auto-update.timer`,
   `podman-clean-transient.service`) are disabled where they exist (and stopped, except `podman-restart.service`, see the
   warning below); the system user `task-runner` (home = state dir, no login shell); a subordinate uid/gid range of 65536 if the user has
   none (above the highest existing range); the binary, built on the control host once per architecture in the play
   (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64 go build -trimpath -buildvcs=false`), copied to the node; the unit.
3. **Guard**: the nftables table and its units (below).
4. **Image**: `podman image exists <tag>`; if missing, copies `image/Containerfile` and `image/host.js` to a temp dir in
   the state dir and runs, as the service user,
   `podman --cgroup-manager=cgroupfs build --pull=missing --build-arg BASE=<digest> --tag localhost/task-runner:b-<hash>`
   (async). The tag is the first 16 hex of sha256(Containerfile, host.js, base digest), so a change rebuilds and a rerun
   with the same inputs does nothing. With `task_runner_image_archive` the tarball is copied, `podman load`ed (it must
   hold exactly one image) and tagged `localhost/task-runner:a-<sha256 of the archive>`.
5. **Config**: the env file (`RUNNER_TOKEN`, `RUNNER_LISTEN`, `RUNNER_STATE_DIR`, `RUNNER_CONTAINER_BIN`, `RUNNER_IMAGE`,
   `RUNNER_MEMORY`, `RUNNER_PIDS_LIMIT`, plus the two `RUNNER_SESSION_*` lines when `task_runner_session_enabled`), a check that the image named there exists, handlers (daemon-reload,
   guard reload, runner restart when the binary, unit or env file changed), enable and start. Then old image tags are
   untagged, keeping the current and the newest other one (`podman rmi` without force, failures ignored); the base
   image stays, a rebuild reuses it.

Every podman call of the role passes the global `--cgroup-manager=cgroupfs` and the service's `HOME` and
`XDG_RUNTIME_DIR`: podman records the run root in its database and refuses a mismatch, so the role and the service
must agree. The runner adds the flag to its own calls.

## Browser sessions

An admin can open an interactive browser on a node (noVNC in a container, started and stopped from node-control's
Browser page). It is off by default. To enable it on a node:

```yaml
task_runner_session_enabled: true
task_runner_session_port: 6080      # optional, the default
```

The env file then also carries `RUNNER_SESSION_BIND` (the node's tailnet address, always a specific IP) and
`RUNNER_SESSION_PORT`. Preflight, only when enabled, checks the port is 1-65535 and differs from `task_runner_port`.
Redeploy restarts the service, so do it when the runner is idle.

- **Tag.** The node must carry the session tag (node-control's `SESSIONS_TAG`, for example `tag:browser-session`), in
  addition to the runner tag; node-control never contacts a node without it. Tag the node with the `SESSIONS_TAG` value, as for the runner tag.
- **ACL.** The tailnet ACL must let the operator (the identity that opens the page's link) reach the session tag on the
  session port (`task_runner_session_port`), besides node-control reaching the runner port. Nothing else needs to reach
  that port. The ACL is the network gate: keep that rule narrow. Each session also has a random token (below) that
  the link carries.
- **Guard.** No change to the nftables guard. Inbound replies on connections the runner accepted on its tailnet address
  are covered by the existing `ct state established,related` accept, and enabling sessions does not change egress
  policy: the session's browser runs as the same service user, so it reaches the public internet only, never the node,
  the LAN or the tailnet.
- **Image.** Sessions use the same run image as runs (it carries the session tools). Its tag is a content hash, so the
  image is rebuilt automatically when the inputs change; the first rebuild after upgrading to a version with session
  support is long (base pull and build, see First pull).
- **Slot.** A session shares the single slot with runs: one at a time, `409` otherwise. A running session makes the
  node report `busy`, so scheduled tasks pinned to that node fail with "node busy" or "no eligible node" until the
  session ends (up to its length).
- **Package.** The role installs `catatonit` besides podman: sessions run podman with `--init`, which needs it.
- **Trust model.** Reachability is gated by the ACL; the session token (random per session, carried in the link, dies
  with the session) stops other pages and users on allowed devices from using the endpoint. The token appears in the
  operator's browser history and in the page's address bar while the session is open, so do not share the link;
  stopping the session invalidates it.
- **Link.** The Open link uses the node's tailnet name when it resolves on the operator's device (MagicDNS), else the
  tailnet IPv4. The published port follows `task_runner_listen_addr`; an IPv6-only listen address is not supported by
  the link. The link can show before noVNC is listening: a click in the first seconds may be refused, retry.
  Closing the last browser window ends the session (reported as failed).

Checks after the first deploy with sessions on: `grep RUNNER_SESSION /etc/task-runner/env` as root shows the tailnet
address and the port; `ss -ltn` shows nothing listening on the session port while idle; start a session from the
Browser page, then `ss -ltn` shows it bound to the tailnet address only (not `0.0.0.0` or `::`), and the link opens
from an operator device the ACL allows and does not from one it does not.

## Facts from the spike

Measured by hand on one candidate node (Ubuntu 26.04, x86_64, 2 vCPU, about 3.9 GB RAM) before the role existed:

- `apt install podman uidmap passt` (pulling in netavark, conmon, crun, libsubid and friends) gives podman 5.7.0; the
  registry and the package hosts were reachable.
- Rootless podman for a service user needs the global `--cgroup-manager=cgroupfs`; the default systemd manager fails
  without a login session.
- DNS inside a rootless container works by inheriting the host's resolvers: the container's `resolv.conf` held pasta's
  forwarder (`169.254.1.1`) and the host's tailnet resolver (`100.100.100.100`); the forwarded queries leave the host
  as the service user's sockets (hence the resolver exception in the guard).
- The first pull of the base image (about 2.5 GB) took about 30 minutes.
- The spike enabled lingering for the user; the role does not (see below).

## First pull

The base image is about 2.5 GB; its first pull took about 30 minutes on a 2 vCPU node in the spike. The build task
runs async (`task_runner_image_async`, default one hour) and polls every 30 s, so the play looks idle meanwhile. On a
slow link raise the limit, or build elsewhere and use `task_runner_image_archive`:

```sh
podman build --build-arg BASE=<image>@sha256:<digest> -t task-run apps/task-runner/image
podman save -o task-run.tar task-run
```

## Base image digest

Use the tag the pinned `playwright` package in `image/Containerfile` matches (see `apps/task-runner/README.md`) and
resolve it to a digest on any machine with registry access, then pin that in the private repo:

```sh
skopeo inspect --format '{{.Digest}}' docker://<image>:<tag>
# or
podman pull <image>:<tag> && podman image inspect --format '{{.Digest}}' <image>:<tag>
# multi-arch: the index digest is in `podman manifest inspect <image>:<tag>` (or the registry's web UI)
```

The index digest of a multi-arch image works for every architecture. A tag or a missing digest fails preflight.

## Service hardening

`User=task-runner`, `PrivateTmp`, `ProtectHome` (HOME is the state dir), `ProtectSystem=strict` with
`ReadWritePaths=` the state and runtime dirs, `ProtectControlGroups=no` (cgroupfs writes
below the unit), `Delegate=yes` with `DelegateSubgroup=runner` (the user manages the cgroup subtree, for the container's memory and pids limits;
the runner lives in the `runner` sub-cgroup so the unit cgroup can hand controllers down; needs systemd >= 254),
`MemoryMax`, `TasksMax`, `RuntimeDirectory=task-runner` (0700, preserved across restarts),
`KillMode=mixed` with `TimeoutStopSec=45` (the runner shuts down within 30 s, killing its container by name),
`Restart=on-failure`. Two `ExecStartPre` steps: one (as root) refuses the start when the guard table has no rule for
the CURRENT uid of the user (fail closed after the user was recreated: rerun the role with `--tags guard`), one waits
up to 60 s for the tailnet address to be configured; the runner binds to it only. No `UMask=` (it would override the
runner's explicit 0755/0644 modes on its run dirs). No access log. The unit `Requires=` the guard.

`ProtectKernelTunables=` is not applied by default (`task_runner_protect_kernel_tunables`): its read-only mounts over
`/proc/sys` and friends become locked mounts in the container's user namespace, and the kernel then refuses the fresh
`/proc` mount every run needs. Try it only on one node after a verified run, and revert on a proc mount error.

**Never add a seccomp-based option** without a verified run: under `User=` systemd silently turns on
`NoNewPrivileges=` for `SystemCallFilter=`, `RestrictNamespaces=`, `PrivateDevices=`, `ProtectKernelModules=`,
`ProtectKernelLogs=`, `ProtectClock=`, `RestrictSUIDSGID=`, `LockPersonality=` and `MemoryDenyWriteExecute=`, which
recreates the `newuidmap` failure below.

**`NoNewPrivileges=` is not set** (nor `RestrictSUIDSGID=`): rootless podman runs the setuid `newuidmap`/`newgidmap`
to map the subordinate ranges, and `no_new_privs` makes that fail. The spec text mentions `NoNewPrivileges`; this
role deviates on purpose. The container itself runs with `--security-opt no-new-privileges --cap-drop=ALL`.

**Lingering is not enabled.** No user systemd instance is needed: podman uses cgroupfs and gets `XDG_RUNTIME_DIR`
from the unit. The spike ran with lingering on, so this is a rollout check (below).

## Guard

`task-runner-guard.service` (oneshot, `After=network-pre.target`, `Before=task-runner.service`) loads table
`inet task_runner_guard` from `/etc/task-runner-guard.nft`. Its `output` chain jumps for `meta skuid <uid of
task-runner>` (numeric, looked up by the role; it refuses uid 0) to a chain that:

1. accepts `ct state established,related` (replies on connections the runner accepted on its tailnet address; a
   connection the user opened is never established unless its first packet passed);
2. accepts UDP and TCP port 53 to `task_runner_guard_resolvers`;
3. drops every address configured on an interface of this host (`fib daddr type local`, IPv4 and IPv6): loopback and
   the node's own addresses on any interface, so a `0.0.0.0`/`::` listener cannot be reached through them, nor through
   pasta's `host.containers.internal` mapping (podman 5 maps 169.254.1.2 in the container to the host). A public address
   that is NOT on an interface (a cloud's 1:1 NAT: `ip -o addr` shows only the private address) is not local to the
   kernel and is not covered: list it in `task_runner_guard_extra_drop`, whose entries are dropped right after this rule;
4. drops everything to `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`,
   `192.168.0.0/16`, `224.0.0.0/4`, `255.255.255.255`, `::1`, `fc00::/7` (covers the tailnet's `fd7a:115c:a1e0::/48`),
   `fe80::/10` and `ff00::/8`;
5. accepts the rest (the public internet).

The resolver accepts come before the local drop, so a listed stub such as `127.0.0.53` still answers.

Rootless podman's user-space networking (pasta) opens the containers' connections on the host as the service user, so
the one uid rule covers the runner, the build and every run. A script therefore cannot reach the ingress agent, the
proxy's local ports, other nodes or the LAN, even through a hostname that resolves to such an address. Containers
resolve through pasta's forwarder, which queries the host's resolvers as the service user: preflight fails if
`/etc/resolv.conf` lists a blocked-range resolver (for example the `127.0.0.53` stub) that is not in
`task_runner_guard_resolvers`. Other users (root, tailscaled, the other services) are untouched.

The rule set is validated with `nft -c -f` before it replaces the file. A change reloads the table atomically
(`systemctl reload task-runner-guard`, never restart, which would restart the runner). Stopping the guard deletes the
table and stops the runner. The uid is resolved by the role: if the user is ever recreated with another uid, rerun the
role (the runner refuses to start until then).

**Limits.** `nft flush ruleset` (or a firewall tool that flushes) deletes the table while the unit stays active. As
for the ingress guard, `task-runner-guard-check.timer` reapplies it within `task_runner_guard_check_interval` (logged
with `journalctl -t task-runner-guard-check`); the gap is at most one interval. Unlike `xray.service`, the runner unit
has no `IPAddressDeny=` second layer: it filters both directions for the whole cgroup, so it would also cut the API on
the tailnet address, and with the containers in the same cgroup it would hit their internal traffic (pasta's DNS
forwarder sits at a link-local address inside the container). The addresses configured on the host's interfaces are
covered by the `fib` rule; a NAT'd public address that is not on an interface is covered only when it is listed in
`task_runner_guard_extra_drop` (otherwise a container can hairpin to the host's public listeners through it). Besides
the flush window and that case, the only traffic the guard lets through to a blocked address is a flow that was
already established before the rule set was (re)loaded.

## Shared hosts

The role disables the package's root-level `podman-restart.service` (`enabled: false`) but deliberately does NOT stop it:
stopping that unit stops every root container that uses `--restart=always`, which would hit other workloads on a host
that also runs root containers. If the unit is running when the role first runs, it stays running until the next boot
or until you stop it yourself. The other three package units are disabled and stopped.

## Smaller nodes

The spike node had 2 vCPU and about 3.9 GB RAM. Chromium needs a few hundred MB per run. On a node with less memory:
lower `task_runner_container_memory` (and `task_runner_memory_max`, keeping 256 MB between them), and lower
`task_runner_min_free_mb` only after measuring a real run (`GET /v1/health` reports `mem_available_mb`; watch
`systemctl status task-runner` for the peak). A node that cannot afford it should not be in the group. Disk: the base
image and the derived image share layers; plan for about 4 GB in `/var/lib/task-runner`.

## Rollout checks on a real node

Run after the first deploy, as root on the node. Steps 7 and 8 are a **hard gate**: do not enable tasks on the node
until the container limits are confirmed.

1. Hardening is as intended: `systemctl show task-runner -p NoNewPrivileges` prints `NoNewPrivileges=no`.
2. Services: `systemctl status task-runner task-runner-guard task-runner-guard-check.timer`;
   `journalctl -u task-runner -n 50` shows `listening` with the tailnet address.
3. Guard: `nft list table inet task_runner_guard` shows the chain; the uid in `meta skuid` equals `id -u task-runner`.
   `ip -o addr` shows the node's public address on an interface; if it does not (1:1 NAT), add it to
   `task_runner_guard_extra_drop`, rerun with `--tags guard` and check the table lists it.
   `nft list ruleset | grep -c table` before and after the deploy differs only by the new table.
4. Root-level podman units are off: `systemctl is-enabled podman.socket podman-restart.service
   podman-auto-update.timer podman-clean-transient.service` prints `disabled` (or "not found") for each.
5. Podman setup: `sudo -u task-runner env HOME=/var/lib/task-runner XDG_RUNTIME_DIR=/run/task-runner podman
   --cgroup-manager=cgroupfs info --format '{{.Host.CgroupManager}} {{.Host.CgroupsVersion}} {{.Store.RunRoot}}'`
   prints `cgroupfs v2` and a run root under `/run/task-runner`.
6. AppArmor: `journalctl -k | grep -i 'apparmor.*DENIED'` is empty after a run (see Not verified, item 2).
7. Start a harmless run through node-control (or `curl` with the token from a peer allowed by the ACL). During the run:
   `find /sys/fs/cgroup/system.slice/task-runner.service -name memory.max -exec grep -H . {} +` and the same for
   `pids.max` show the container's `task_runner_container_memory` and `task_runner_pids_limit`
   (`systemd-cgls -u task-runner.service` locates the container cgroup).
8. If step 7 shows no container limit (the container sees the unit's `MemoryMax`/`TasksMax` instead), check that the unit
   has `DelegateSubgroup=runner` and that `cat /sys/fs/cgroup/system.slice/task-runner.service/cgroup.subtree_control`
   lists `memory pids`. Without the sub-cgroup the runner sits in the delegated cgroup itself, controllers are not
   passed down, and the unit's limits bound the whole subtree (runner plus container) but one run can use all of it.
9. DNS and the guard from inside a container: a run whose script prints `/etc/resolv.conf` (via `fs`) and resolves
   and opens `https://example.com` succeeds; runs that fetch `http://169.254.169.254/`, `http://<node tailnet ip>:9100/`,
   `http://10.0.0.1/`, `http://host.containers.internal:22/` and `http://<node public ip>:22/` all fail (time out).
   As the user on the host: `sudo -u task-runner curl -sS -m 5 http://127.0.0.1:22/` fails,
   `sudo -u task-runner curl -sS -m 5 -o /dev/null -w '%{http_code}\n' https://example.com/` succeeds.
10. Stop during a run: start a long run, `systemctl stop task-runner`, then `sudo -u task-runner env
    HOME=/var/lib/task-runner XDG_RUNTIME_DIR=/run/task-runner podman --cgroup-manager=cgroupfs ps -a` is empty.
11. Flush (test node only): `nft flush ruleset`, and the table is back within the check interval
    (`journalctl -t task-runner-guard-check`).
12. Runner tag (from the operator machine): `scripts/headscale-tag-node.sh show --node <given-name>` first, read the
    printed model, then `add --node <given-name> --tag tag:task-runner` without `--yes`, check the plan, then apply.

## Not verified against a real host

Only static checks were run (syntax check, template rendering). Unverified:

1. Podman works for the service without lingering (spike used lingering), with `XDG_RUNTIME_DIR=/run/task-runner`
   from `RuntimeDirectory=` and the role's own calls via `sudo` using the same run root.
2. AppArmor: Ubuntu confines pasta/passt with a profile, and the runtime dir `/run/task-runner` (no lingering, no
   `/run/user/<uid>`) may fall outside what it allows. Check `journalctl -k | grep -i 'apparmor.*DENIED'` after a run.
   Fallback (a role change: `task_runner_runtime_dir` only accepts `/run/<dir>` today): enable lingering for the user
   (`loginctl enable-linger task-runner`) and point `XDG_RUNTIME_DIR` at `/run/user/<uid>` instead of the unit's
   `RuntimeDirectory`.
3. The sandbox is compatible with rootless podman with `ProtectSystem=strict` (and `ProtectKernelTunables` off).
4. `Delegate=yes` with `DelegateSubgroup=runner` makes the container's `--memory`/`--pids-limit` effective under cgroupfs (see the check above).
5. The subordinate id ranges added by the role are picked up (`podman unshare cat /proc/self/uid_map` as the user).
   If ranges are ever changed after the storage exists, run `podman system migrate` as the user.
6. The guard (including the `fib` rule) leaves the build (registry, npm) and runs working, DNS included, and blocks the private ranges.
7. The async build/load with `become_user` (needs `acl`), the image pruning, and the archive path.
8. The fail-closed uid check in `ExecStartPre` (the `nft -n` output format for `skuid`).
9. `KillMode=mixed` stop behaviour during an active run (the container is gone afterwards: `podman ps -a` as the user).

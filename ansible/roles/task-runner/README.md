# task-runner

This role deploys the generic task runner (`apps/task-runner/` in this repo, see its README for the API) on a VM node as
`task-runner.service`. The role sets up these items:

- an unprivileged service user
- rootless podman
- the run image (Chromium + Playwright + `host.js` of the runner)
- the env file with the token
- an nftables guard that keeps the service user, and so every container that it starts, off loopback, the LAN and the tailnet

node-control calls the runner over the tailnet.

Not verified against a real host yet. Before the first deploy, read [Not verified](#not-verified-against-a-real-host).

## Use

There is no playbook in this repo (like the other node roles). The private repo applies the role to an inventory group,
after the base `tailscale` role:

```yaml
- name: Task runners
  hosts: task_runners
  roles:
    - role: task-runner
      tags: [task-runner]
```

Site values come from host_vars or the vault in the private repo, never from this repo:

```yaml
task_runner_src: "{{ playbook_dir }}/../vendor/headlamp/apps/task-runner"
task_runner_token: "{{ vault_task_runner_token }}"          # openssl rand -hex 32
task_runner_listen_addr: "<tailnet-ip>"                       # `tailscale ip -4` on the node
task_runner_base_image: "<registry>/<repository>@sha256:<64 hex>"
```

Give the node the runner tag in headscale (`TASKS_RUNNER_TAG` of node-control, default `tag:task-runner`).
node-control never contacts an untagged node. In the tailnet ACL, allow the runner port (`task_runner_port`, default 9100) to that tag from node-control
only.

**WARNING:** On the unified model, tagging a node that has no tags removes its user ownership. Only authentication again with empty tags undoes this.
For this reason, the helper also asks for `--allow-ownership-change`.

Tag the node as an admin:

1. Run `scripts/headscale-tag-node.sh show --node <given-name>`. Read the tag model that it prints. The model is `unified` on headscale 0.28+ (one `tags` list). Before that, it is `legacy` (forced and valid
   tags).
2. Run `add --node <given-name> --tag tag:task-runner` without `--yes` to see the plan.
3. Run the same command with `--yes`.
4. Run `show` again to verify.

`headscale nodes tag` or a pre-auth key with that ACL tag work too.

**WARNING:** The `tagOwners` of the policy decides who can have the tag at registration (and, on legacy servers, who can request it later).
That entry is the trust boundary. List only the admin or service
identity as owner of the runner tag. Never list human users whose devices are not runners.

**systemd-resolved hosts:** If `/etc/resolv.conf` points at the local stub (`nameserver 127.0.0.53`), add
`127.0.0.53` to `task_runner_guard_resolvers`. Keep the tailnet entries if you use them. Preflight fails safe when a
blocked-range resolver is missing. The play stops there until the list is right.

**CAUTION: Deploy when the runner is idle.** A rerun that changes the binary, the unit or the env file restarts the service.
The restart cancels an active run, and node-control gets a cancelled report. Check `busy` in `GET /v1/health` first.

Tags: `task-runner` (everything), `install`, `guard`, `image`, `config`, `image-prune` (only the pruning of old image
tags). Preflight has the tag `always`, so it runs under every `--tags`. `--skip-tags` of any other
tag does not drop it. Only `--skip-tags always` removes it.

`--tags install` alone installs the binary and the unit and notifies the restart
before `config` writes the env file. On a fresh node, run the whole role (no tags) the first time.

Check mode on the node is safe: the role templates files. It builds or loads no image, and it starts or reloads nothing.
The Go binary IS built on the control host (into a temp dir, `check_mode: false`), so the role can compare the copy. Nothing
reaches the node. On a fresh host, the steps that need the service user can fail in check mode (as in the other roles).

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

The layout is fixed. Variables exist for it, but change them only with reason:

- user `task-runner`
- state dir and HOME `/var/lib/task-runner` (0700)
- runtime dir `/run/task-runner` (0700)
- env file `/etc/task-runner/env` (0640 `root:task-runner`)
- binary `/usr/local/bin/task-runner`
- guard file `/etc/task-runner-guard.nft`
- image repository `localhost/task-runner`

## What it does

1. **Preflight** (read-only). It checks these items:
   - facts, Debian family with apt, x86_64 or aarch64
   - required variables and token shape
   - listen address in the tailnet ranges and present on the node
   - limits, resolvers and `task_runner_guard_extra_drop`
   - base image digest
   - the source on the control host, and Go (unless `task_runner_binary`)
   - free memory and disk
   - every blocked-range resolver of `/etc/resolv.conf` is listed in `task_runner_guard_resolvers`

   Free memory fails the play only when the service is not running yet. Otherwise it gives a warning.
   Free disk is checked in the image step, only when the role has to build or load the image.

   Preflight warns (never fails) when the inventory address (`ansible_host`) is an IP that is not on any interface of the
   node and not in `task_runner_guard_extra_drop`. This is a likely public address from 1:1 NAT that the guard would not cover.
   That warning sees only `ansible_host`. When the inventory reaches nodes by their tailnet address (the usual case here), it
   never fires. The `ip -o addr` check in "Rollout checks" (step 3) is then the real control for a public address from NAT.

   Preflight fails when `task_runner_guard_extra_drop` has an octet above 255 or overlapping or duplicate entries.
   nft refuses overlapping intervals in one set. Preflight compares IPv4 entries as ranges and IPv6 entries only for exact duplicates.
   Preflight also works out the image tag.
2. **Install**:
   - Install `podman`, `uidmap`, `passt` and `acl` (Ansible needs `setfacl` to run commands as the service user).
   - Disable the root-level units of the package where they exist: `podman.socket`, `podman-restart.service`, `podman-auto-update.timer`,
     `podman-clean-transient.service`. Stop them, except `podman-restart.service` (see "Shared hosts").
   - Create the system user `task-runner` (home = state dir, no login shell).
   - Add a subordinate uid/gid range of 65536 if the user has none (above the highest existing range).
   - Build the binary on the control host, once per architecture in the play
     (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64|arm64 go build -trimpath -buildvcs=false`). Copy it to the node.
   - Install the unit.
3. **Guard**: the nftables table and its units (below).
4. **Image**: run `podman image exists <tag>`. If the image is missing, copy `image/Containerfile`, `image/host.js` and `image/session.sh` to a temp dir in
   the state dir. Then run, as the service user (async),
   `podman --cgroup-manager=cgroupfs build --pull=missing --build-arg BASE=<digest> --tag localhost/task-runner:b-<hash>`.
   The tag is the first 16 hex of sha256(Containerfile, host.js, session.sh, base digest). A change rebuilds, and a rerun
   with the same inputs does nothing. With `task_runner_image_archive`, the role copies the tarball and runs `podman load` on it (it must
   hold exactly one image). It tags the image `localhost/task-runner:a-<sha256 of the archive>`.
5. **Config**: write the env file (`RUNNER_TOKEN`, `RUNNER_LISTEN`, `RUNNER_STATE_DIR`, `RUNNER_CONTAINER_BIN`, `RUNNER_IMAGE`,
   `RUNNER_MEMORY`, `RUNNER_PIDS_LIMIT`, plus the two `RUNNER_SESSION_*` lines when `task_runner_session_enabled`).
   Check that the image named there exists. Run the handlers: daemon-reload, guard reload, and runner restart when the binary, unit or env file changed.
   Enable and start the service. Then untag old image tags, and keep the current tag and the newest other one (`podman rmi` without force, failures ignored).
   The base image stays, because a rebuild reuses it.

Every podman call of the role passes the global `--cgroup-manager=cgroupfs` and the `HOME` and
`XDG_RUNTIME_DIR` of the service. Podman records the run root in its database and refuses a mismatch, so the role and the service
must agree. The runner adds the flag to its own calls.

## Browser sessions

An admin can open an interactive browser on a node (noVNC in a container). The Browser page of node-control starts and stops it.
It is off by default. To enable it on a node, set:

```yaml
task_runner_session_enabled: true
task_runner_session_port: 6080      # optional, the default
```

The env file then also carries `RUNNER_SESSION_BIND` (the tailnet address of the node, always a specific IP) and
`RUNNER_SESSION_PORT`. Only when sessions are enabled, preflight checks that the port is 1-65535 and differs from `task_runner_port`.
A redeploy restarts the service, so do it when the runner is idle.

- **Tag.** The node must carry the session tag, in
  addition to the runner tag. The session tag is the `SESSIONS_TAG` of node-control, for example `tag:browser-session`.
  node-control never contacts a node without it. Tag the node with the `SESSIONS_TAG` value, as for the runner tag.
- **ACL.** The tailnet ACL must let the operator reach the session tag on the
  session port (`task_runner_session_port`). The operator is the identity that opens the link of the page. node-control also reaches the runner port. Nothing else needs to reach
  the session port. The ACL is the network gate, so keep that rule narrow. Each session also has a random token (below) that
  the link carries.
- **Guard.** No change to the nftables guard. The existing `ct state established,related` accept covers inbound replies on connections that the runner accepted on its tailnet address.
  Sessions do not change the egress policy. The browser of the session runs as the same service user.
  It reaches the public internet only, never the node, the LAN or the tailnet.
- **Image.** Sessions use the same run image as runs (it carries the session tools). Its tag is a content hash, so the
  role rebuilds the image automatically when the inputs change. The first rebuild after you upgrade to a version with session
  support is long (base pull and build, see First pull).
- **Slot.** A session shares the single slot with runs: one at a time, `409` otherwise. A running session makes the
  node report `busy`. Scheduled tasks pinned to that node then fail with "node busy" or "no eligible node" until the
  session ends (up to its length).
- **Package.** The role installs `catatonit` besides podman. Sessions run podman with `--init`, which needs it.
- **Trust model.** The ACL controls reachability. The session token stops other pages and users on allowed devices from using the endpoint.
  The token is random for each session, it is in the link, and it dies with the session.
  The token appears in the browser history of the operator and in the address bar of the page while the session is open.
  Do not share the link. Stopping the session invalidates the token.
- **Link.** The Open link uses the tailnet name of the node when it resolves on the device of the operator (MagicDNS). Otherwise it uses the
  tailnet IPv4. The published port follows `task_runner_listen_addr`. The link does not support an IPv6-only listen address.
  The link can show before noVNC is listening. A click in the first seconds can be refused, so retry.
  Closing the last browser window reopens Chrome. A session ends by Stop, its length, the idle timeout, or if its display or bridge dies.

Checks after the first deploy with sessions on:

1. As root, `grep RUNNER_SESSION /etc/task-runner/env` shows the tailnet address and the port.
2. `ss -ltn` shows nothing that listens on the session port while the session is idle.
3. Start a session from the Browser page. Then `ss -ltn` shows it bound to the tailnet address only (not `0.0.0.0` or `::`).
4. The link opens from an operator device that the ACL allows. It does not open from a device that the ACL does not allow.

## Facts from the spike

Measured by hand on one candidate node (Ubuntu 26.04, x86_64, 2 vCPU, about 3.9 GB RAM) before the role existed:

- `apt install podman uidmap passt` (this pulls in netavark, conmon, crun, libsubid and friends) gives podman 5.7.0. The
  registry and the package hosts were reachable.
- Rootless podman for a service user needs the global `--cgroup-manager=cgroupfs`. The default systemd manager fails
  without a login session.
- DNS inside a rootless container works because the container inherits the resolvers of the host. The `resolv.conf` of the container held the forwarder of pasta
  (`169.254.1.1`) and the tailnet resolver of the host (`100.100.100.100`). The forwarded queries leave the host
  as sockets of the service user. For this reason, the guard has a resolver exception.
- The first pull of the base image (about 2.5 GB) took about 30 minutes.
- The spike enabled lingering for the user. The role does not (see below).

## First pull

The base image is about 2.5 GB. Its first pull took about 30 minutes on a 2 vCPU node in the spike. The build task
runs async (`task_runner_image_async`, default one hour) and polls every 30 s, so the play looks idle meanwhile. On a
slow link, raise the limit. Or build elsewhere and use `task_runner_image_archive`:

```sh
podman build --build-arg BASE=<image>@sha256:<digest> -t task-run apps/task-runner/image
podman save -o task-run.tar task-run
```

## Base image digest

Use the tag that the pinned `playwright` package in `image/Containerfile` matches (see `apps/task-runner/README.md`).
Resolve it to a digest on any machine with registry access. Then pin that digest in the private repo:

```sh
skopeo inspect --format '{{.Digest}}' docker://<image>:<tag>
# or
podman pull <image>:<tag> && podman image inspect --format '{{.Digest}}' <image>:<tag>
# multi-arch: the index digest is in `podman manifest inspect <image>:<tag>` (or the registry's web UI)
```

The index digest of a multi-arch image works for every architecture. A tag or a missing digest fails preflight.

## Service hardening

The unit has these settings:

- `User=task-runner`, `PrivateTmp`, `ProtectHome` (HOME is the state dir).
- `ProtectSystem=strict` with `ReadWritePaths=` the state and runtime dirs.
- `ProtectControlGroups=no`, because cgroupfs writes below the unit.
- `Delegate=yes` with `DelegateSubgroup=runner`. The user manages the cgroup subtree, for the memory and pids limits of the container.
  The runner lives in the `runner` sub-cgroup, so the unit cgroup can hand controllers down. This needs systemd >= 254.
- `MemoryMax`, `TasksMax`.
- `RuntimeDirectory=task-runner` (0700, kept across restarts).
- `KillMode=mixed` with `TimeoutStopSec=45`. The runner shuts down within 30 s and kills its container by name.
- `Restart=on-failure`.
- Two `ExecStartPre` steps. One step (as root) refuses the start when the guard table has no rule for
  the CURRENT uid of the user. This fails closed after someone recreated the user. In that case, rerun the role with `--tags guard`.
  The other step waits up to 60 s for the tailnet address to be configured. The runner binds to it only.
- No `UMask=`, because it would override the explicit 0755/0644 modes of the runner on its run dirs.
- No access log.
- The unit `Requires=` the guard.

`ProtectKernelTunables=` is not applied by default (`task_runner_protect_kernel_tunables`). Its read-only mounts over
`/proc/sys` and friends become locked mounts in the user namespace of the container. The kernel then refuses the fresh
`/proc` mount that every run needs. Try it only on one node after a verified run. Revert on a proc mount error.

**WARNING:** Never add a seccomp-based option without a verified run. Under `User=`, systemd silently turns on
`NoNewPrivileges=` for these options: `SystemCallFilter=`, `RestrictNamespaces=`, `PrivateDevices=`, `ProtectKernelModules=`,
`ProtectKernelLogs=`, `ProtectClock=`, `RestrictSUIDSGID=`, `LockPersonality=` and `MemoryDenyWriteExecute=`. This
recreates the `newuidmap` failure below.

**`NoNewPrivileges=` is not set** (nor `RestrictSUIDSGID=`). Rootless podman runs the setuid `newuidmap` and `newgidmap`
to map the subordinate ranges, and `no_new_privs` makes that fail. The spec text mentions `NoNewPrivileges`. This
role deviates on purpose. The container itself runs with `--security-opt no-new-privileges --cap-drop=ALL`.

**Lingering is not enabled.** No user systemd instance is needed. Podman uses cgroupfs and gets `XDG_RUNTIME_DIR`
from the unit. The spike ran with lingering on, so this is a rollout check (below).

## Guard

`task-runner-guard.service` (oneshot, `After=network-pre.target`, `Before=task-runner.service`) loads table
`inet task_runner_guard` from `/etc/task-runner-guard.nft`. Its `output` chain jumps for `meta skuid <uid of
task-runner>` to a chain. The uid is numeric, and the role looks it up. The role refuses uid 0. The chain does this:

1. It accepts `ct state established,related`. These are replies on connections that the runner accepted on its tailnet address.
   A connection that the user opened is never established unless its first packet passed.
2. It accepts UDP and TCP port 53 to `task_runner_guard_resolvers`.
3. It drops every address configured on an interface of this host (`fib daddr type local`, IPv4 and IPv6). This covers loopback and
   the own addresses of the node on any interface. A `0.0.0.0` or `::` listener cannot be reached through them.
   It cannot be reached through the `host.containers.internal` mapping of pasta either.
   Podman 5 maps 169.254.1.2 in the container to the host.
4. It drops everything to these IPv4 ranges: `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`,
   `192.168.0.0/16`, `224.0.0.0/4` and `255.255.255.255`. It drops also these IPv6 ranges: `::1`, `fc00::/7`, `fe80::/10` and `ff00::/8`.
   The range `fc00::/7` covers `fd7a:115c:a1e0::/48` of the tailnet.
5. It accepts the rest (the public internet).

A public address that is NOT on an interface is not local to the kernel, and the `fib` rule does not cover it.
An example is a cloud with 1:1 NAT: `ip -o addr` shows only the private address. List such an address in `task_runner_guard_extra_drop`.
The guard drops its entries right after the `fib` rule.

The resolver accepts come before the local drop. A listed stub such as `127.0.0.53` therefore still answers.

Rootless podman uses user-space networking (pasta). Pasta opens the connections of the containers on the host as the service user.
The one uid rule therefore covers the runner, the build and every run.

A script cannot reach the ingress agent, the
local ports of the proxy, other nodes or the LAN. This is also true through a hostname that resolves to such an address.

Containers resolve through the forwarder of pasta. The forwarder queries the resolvers of the host as the service user.
Preflight fails if `/etc/resolv.conf` lists a blocked-range resolver (for example the `127.0.0.53` stub) that is not in
`task_runner_guard_resolvers`. Other users (root, tailscaled, the other services) are not affected.

The role validates the rule set with `nft -c -f` before it replaces the file. A change reloads the table atomically
(`systemctl reload task-runner-guard`). Never restart it, because that restarts the runner.

Stopping the guard deletes the table and stops the runner. The role resolves the uid. If someone recreates the user with another uid, rerun the
role. The runner refuses to start until then.

**Limits.** `nft flush ruleset` (or a firewall tool that flushes) deletes the table while the unit stays active. As
for the ingress guard, `task-runner-guard-check.timer` applies the table again within `task_runner_guard_check_interval` (logged
with `journalctl -t task-runner-guard-check`). The gap is at most one interval.

Unlike `xray.service`, the runner unit has no `IPAddressDeny=` second layer. That layer filters both directions for the whole cgroup,
so it would also cut the API on the tailnet address. The containers are in the same cgroup, so it would hit their internal traffic.
The DNS forwarder of pasta sits at a link-local address inside the container.

The `fib` rule covers the addresses configured on the interfaces of the host. A public address from NAT that is not on an interface is covered only when you list it in
`task_runner_guard_extra_drop`. Otherwise a container can hairpin to the public listeners of the host through it.

Besides the flush window and that case, the guard lets traffic through to a blocked address in one case only.
That case is a flow that was already established before the rule set was (re)loaded.

## Shared hosts

The role disables the root-level `podman-restart.service` of the package (`enabled: false`) but deliberately does NOT stop it.
Stopping that unit stops every root container that uses `--restart=always`. This would hit other workloads on a host
that also runs root containers. If the unit is running when the role first runs, it stays running until the next boot
or until you stop it yourself. The role disables and stops the other three package units.

## Smaller nodes

The spike node had 2 vCPU and about 3.9 GB RAM. Chromium needs a few hundred MB per run. On a node with less memory:

- Lower `task_runner_container_memory` (and `task_runner_memory_max`, with 256 MB between them).
- Lower `task_runner_min_free_mb` only after you measure a real run. `GET /v1/health` reports `mem_available_mb`. Watch
  `systemctl status task-runner` for the peak.

Do not put a node in the group if it cannot afford this. Disk: the base
image and the derived image share layers. Plan for about 4 GB in `/var/lib/task-runner`.

## Rollout checks on a real node

**WARNING:** Steps 7 and 8 are a hard gate. Do not enable tasks on the node
until you confirm the container limits.

Run these checks after the first deploy, as root on the node.

1. Make sure that the hardening is as intended. `systemctl show task-runner -p NoNewPrivileges` must print `NoNewPrivileges=no`.
2. Services: run `systemctl status task-runner task-runner-guard task-runner-guard-check.timer`.
   `journalctl -u task-runner -n 50` shows `listening` with the tailnet address.
3. Guard: `nft list table inet task_runner_guard` shows the chain. The uid in `meta skuid` equals `id -u task-runner`.
   `ip -o addr` shows the public address of the node on an interface. If it does not (1:1 NAT), add the address to
   `task_runner_guard_extra_drop`. Rerun with `--tags guard`. Check that the table lists the address.
   `nft list ruleset | grep -c table` before and after the deploy differs only by the new table.
4. Root-level podman units are off: `systemctl is-enabled podman.socket podman-restart.service
   podman-auto-update.timer podman-clean-transient.service` prints `disabled` (or "not found") for each.
5. Podman setup: run `sudo -u task-runner env HOME=/var/lib/task-runner XDG_RUNTIME_DIR=/run/task-runner podman
   --cgroup-manager=cgroupfs info --format '{{.Host.CgroupManager}} {{.Host.CgroupsVersion}} {{.Store.RunRoot}}'`.
   It prints `cgroupfs v2` and a run root under `/run/task-runner`.
6. AppArmor: `journalctl -k | grep -i 'apparmor.*DENIED'` is empty after a run (see Not verified, item 2).
7. Start a harmless run through node-control (or `curl` with the token from a peer that the ACL allows). During the run, run
   `find /sys/fs/cgroup/system.slice/task-runner.service -name memory.max -exec grep -H . {} +` and the same for
   `pids.max`. They show `task_runner_container_memory` and `task_runner_pids_limit` of the container
   (`systemd-cgls -u task-runner.service` locates the container cgroup).
8. Step 7 can show no container limit. (The container then sees `MemoryMax` and `TasksMax` of the unit instead.)
   In that case, check that the unit has `DelegateSubgroup=runner`. Check also that `cat /sys/fs/cgroup/system.slice/task-runner.service/cgroup.subtree_control`
   lists `memory pids`. Without the sub-cgroup, the runner sits in the delegated cgroup itself, and controllers are not
   passed down. The limits of the unit then bound the whole subtree (runner plus container). One run can use all of it.
9. DNS and the guard from inside a container. A run whose script prints `/etc/resolv.conf` (through `fs`)
   and resolves and opens `https://example.com` succeeds. Runs that fetch `http://169.254.169.254/`, `http://<node tailnet ip>:9100/`,
   `http://10.0.0.1/`, `http://host.containers.internal:22/` and `http://<node public ip>:22/` all fail (time out).
   As the user on the host, `sudo -u task-runner curl -sS -m 5 http://127.0.0.1:22/` fails.
   `sudo -u task-runner curl -sS -m 5 -o /dev/null -w '%{http_code}\n' https://example.com/` succeeds.
10. Stop during a run: start a long run, then run `systemctl stop task-runner`. After that, `sudo -u task-runner env
    HOME=/var/lib/task-runner XDG_RUNTIME_DIR=/run/task-runner podman --cgroup-manager=cgroupfs ps -a` is empty.
11. Flush (test node only): run `nft flush ruleset`. The table is back within the check interval
    (`journalctl -t task-runner-guard-check`).
12. Runner tag (from the operator machine): run `scripts/headscale-tag-node.sh show --node <given-name>` first and read the
    printed model. Then run `add --node <given-name> --tag tag:task-runner` without `--yes` and check the plan. Then apply it.

## Not verified against a real host

Only static checks were run (syntax check, template rendering). These items are unverified:

1. Podman works for the service without lingering (the spike used lingering), with `XDG_RUNTIME_DIR=/run/task-runner`
   from `RuntimeDirectory=`. The own calls of the role through `sudo` use the same run root.
2. AppArmor: Ubuntu confines pasta/passt with a profile. The runtime dir `/run/task-runner` (no lingering, no
   `/run/user/<uid>`) can be outside what the profile allows. Check `journalctl -k | grep -i 'apparmor.*DENIED'` after a run.
   The fallback is a role change, because `task_runner_runtime_dir` accepts only `/run/<dir>` today.
   Enable lingering for the user
   (`loginctl enable-linger task-runner`) and point `XDG_RUNTIME_DIR` at `/run/user/<uid>` instead of the
   `RuntimeDirectory` of the unit.
3. The sandbox is compatible with rootless podman with `ProtectSystem=strict` (and `ProtectKernelTunables` off).
4. `Delegate=yes` with `DelegateSubgroup=runner` makes `--memory` and `--pids-limit` of the container effective under cgroupfs (see the check above).
5. The role picks up the subordinate id ranges that it adds (`podman unshare cat /proc/self/uid_map` as the user).
   If you change ranges after the storage exists, run `podman system migrate` as the user.
6. The guard (including the `fib` rule) leaves the build (registry, npm) and runs working, DNS included. It blocks the private ranges.
7. The async build/load with `become_user` (needs `acl`), the image pruning, and the archive path.
8. The fail-closed uid check in `ExecStartPre` (the `nft -n` output format for `skuid`).
9. `KillMode=mixed` stop behaviour during an active run (the container is gone afterwards: `podman ps -a` as the user).

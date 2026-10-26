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

Allow the runner port (`task_runner_port`, default 9100) to this node from node-control only, in the tailnet ACL.

Tags: `task-runner` (everything), `install`, `guard`, `image`, `config`, `image-prune` (only the pruning of old image
tags). Preflight runs under every tag. Check mode is safe: files are templated, nothing is built, loaded, started or
reloaded; on a fresh host the steps that need the service user may fail in check mode (as in the other roles).

## Variables

| Variable | Default | Notes |
|---|---|---|
| `task_runner_src` | `""` | Required. Path on the **control host** to `apps/task-runner`. |
| `task_runner_token` | `""` | Required, vault. At least 24 characters, no whitespace, quotes, backslash, `$` or backtick. Never logged. |
| `task_runner_listen_addr` | `""` | Required. The node's tailnet address (100.64.0.0/10, or IPv6 in fd7a:115c:a1e0::/48). Preflight checks `tailscale ip` on the node reports it. |
| `task_runner_port` | `9100` | Bound on `task_runner_listen_addr` only. |
| `task_runner_base_image` | `""` | Required unless `task_runner_image_archive` is set. `<image>@sha256:<64 hex>`, see [Base image digest](#base-image-digest). |
| `task_runner_image_archive` | `""` | Optional control-host path to a `podman save` tarball; loaded instead of building. |
| `task_runner_binary` | `""` | Optional control-host path to a prebuilt linux binary; otherwise built with the control host's `go`. |
| `task_runner_memory_max` | `2G` | systemd `MemoryMax` for the whole service (runner, podman, the container). |
| `task_runner_container_memory` | `1g` | `RUNNER_MEMORY` (podman `--memory`); at least 256 MB below `task_runner_memory_max`. |
| `task_runner_pids_limit` / `task_runner_tasks_max` | `512` / `2048` | Per container (`RUNNER_PIDS_LIMIT`) / whole service (`TasksMax`). |
| `task_runner_min_free_mb` / `task_runner_min_free_disk_mb` | `1500` / `6000` | Preflight: free memory without page cache; free disk on the file system of the state dir. |
| `task_runner_guard_resolvers` | `["100.100.100.100", "fd7a:115c:a1e0::53"]` | Resolvers the service user may query on port 53 although they are in a blocked range. |
| `task_runner_guard_check_interval` | `30s` | How often the guard check timer looks for the table. |
| `task_runner_image_async` / `task_runner_image_poll` | `3600` / `30` | Seconds allowed for a build or load, and the poll interval. |

Fixed layout (variables exist, change only with reason): user `task-runner`, state dir and HOME `/var/lib/task-runner`
(0700), runtime dir `/run/task-runner` (0700), env file `/etc/task-runner/env` (0640 `root:task-runner`), binary
`/usr/local/bin/task-runner`, guard file `/etc/task-runner-guard.nft`, image repository `localhost/task-runner`.

## What it does

1. **Preflight** (read-only): facts, Debian family with apt, x86_64 or aarch64, required variables, token shape, listen
   address in the tailnet ranges and present on the node, limits, resolvers, base image digest, the source on the
   control host, Go (unless `task_runner_binary`), free memory and disk, every blocked-range resolver of
   `/etc/resolv.conf` listed in `task_runner_guard_resolvers`. Works out the image tag.
2. **Install**: `podman`, `uidmap`, `passt`, `acl` (Ansible needs `setfacl` to run commands as the service user); the
   system user `task-runner` (home = state dir, no login shell); a subordinate uid/gid range of 65536 if the user has
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
   `RUNNER_MEMORY`, `RUNNER_PIDS_LIMIT` only), a check that the image named there exists, handlers (daemon-reload,
   guard reload, runner restart when the binary, unit or env file changed), enable and start. Then old image tags are
   untagged, keeping the current and the newest other one (`podman rmi` without force, failures ignored); the base
   image stays, a rebuild reuses it.

Every podman call of the role passes the global `--cgroup-manager=cgroupfs` and the service's `HOME` and
`XDG_RUNTIME_DIR`: podman records the run root in its database and refuses a mismatch, so the role and the service
must agree. The runner adds the flag to its own calls.

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
`ReadWritePaths=` the state and runtime dirs, `ProtectKernelTunables`, `ProtectControlGroups=no` (cgroupfs writes
below the unit), `Delegate=yes` (the user manages the cgroup subtree, for the container's memory and pids limits),
`MemoryMax`, `TasksMax`, `UMask=0077`, `RuntimeDirectory=task-runner` (0700, preserved across restarts),
`KillMode=mixed` with `TimeoutStopSec=45` (the runner shuts down within 30 s, killing its container by name),
`Restart=on-failure`. `ExecStartPre` waits up to 60 s for the tailnet address to be configured; the runner binds to it
only. No access log. The unit `Requires=` the guard.

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
3. drops everything to `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`,
   `192.168.0.0/16`, `::1`, `fc00::/7` (covers the tailnet's `fd7a:115c:a1e0::/48`) and `fe80::/10`;
4. accepts the rest (the public internet).

Rootless podman's user-space networking (pasta) opens the containers' connections on the host as the service user, so
the one uid rule covers the runner, the build and every run. A script therefore cannot reach the ingress agent, the
proxy's local ports, other nodes or the LAN, even through a hostname that resolves to such an address. Containers
resolve through pasta's forwarder, which queries the host's resolvers as the service user: preflight fails if
`/etc/resolv.conf` lists a blocked-range resolver (for example the `127.0.0.53` stub) that is not in
`task_runner_guard_resolvers`. Other users (root, tailscaled, the other services) are untouched.

The rule set is validated with `nft -c -f` before it replaces the file. A change reloads the table atomically
(`systemctl reload task-runner-guard`, never restart, which would restart the runner). Stopping the guard deletes the
table and stops the runner. The uid is resolved by the role: if the user is ever recreated with another uid, rerun the
role.

**Limits.** `nft flush ruleset` (or a firewall tool that flushes) deletes the table while the unit stays active. As
for the ingress guard, `task-runner-guard-check.timer` reapplies it within `task_runner_guard_check_interval` (logged
with `journalctl -t task-runner-guard-check`); the gap is at most one interval. Unlike `xray.service`, the runner unit
has no `IPAddressDeny=` second layer: it filters both directions for the whole cgroup, so it would also cut the API on
the tailnet address, and with the containers in the same cgroup it would hit their internal traffic (pasta's DNS
forwarder sits at a link-local address inside the container). The host's own public address, if it is configured on
an interface, is not covered.

## Smaller nodes

The spike node had 2 vCPU and about 3.9 GB RAM. Chromium needs a few hundred MB per run. On a node with less memory:
lower `task_runner_container_memory` (and `task_runner_memory_max`, keeping 256 MB between them), and lower
`task_runner_min_free_mb` only after measuring a real run (`GET /v1/health` reports `mem_available_mb`; watch
`systemctl status task-runner` for the peak). A node that cannot afford it should not be in the group. Disk: the base
image and the derived image share layers; plan for about 4 GB in `/var/lib/task-runner`.

## Rollout checks on a real node

Run after the first deploy (as root on the node unless noted):

```sh
systemctl status task-runner task-runner-guard task-runner-guard-check.timer
journalctl -u task-runner -n 50                        # "listening" with the tailnet address
nft list table inet task_runner_guard                  # the chain and the uid
systemd-cgls -u task-runner.service                    # runner (and, during a run, conmon/container) below the unit
sudo -u task-runner env HOME=/var/lib/task-runner XDG_RUNTIME_DIR=/run/task-runner \
  podman --cgroup-manager=cgroupfs info --format '{{.Host.CgroupManager}} {{.Host.CgroupsVersion}} {{.Store.RunRoot}}'
# guard: these must time out / fail as the service user, succeed as root
sudo -u task-runner curl -sS -m 5 http://127.0.0.1:22/ ; echo "rc=$?"
sudo -u task-runner curl -sS -m 5 http://<tailnet-ip-of-another-node>:<port>/ ; echo "rc=$?"
sudo -u task-runner curl -sS -m 5 -o /dev/null -w '%{http_code}\n' https://example.com/   # allowed
```

Then start a harmless run through node-control (or `curl` with the token from a tailnet peer allowed by the ACL) whose
script does `await page.goto('https://example.com')` and, while it runs:

- `find /sys/fs/cgroup/system.slice/task-runner.service -name memory.max -exec grep -H . {} +` (or find the
  container's cgroup with `systemd-cgls`) shows the `task_runner_container_memory` limit, and `pids.max` the pids limit. If the run fails with a
  cgroup error or no limit is set, the delegation is not enough: try `DelegateSubgroup=` in the unit (systemd 254+),
  which moves the runner out of the delegated cgroup's root.
- DNS works inside the container (the run reaches the page); a script that fetches `http://10.0.0.1/` or the
  node's tailnet address fails.
- After `nft flush ruleset` (on a test node), the table is back within the check interval.

## Not verified against a real host

Only static checks were run (syntax check, template rendering). Unverified:

1. Podman works for the service without lingering (spike used lingering), with `XDG_RUNTIME_DIR=/run/task-runner`
   from `RuntimeDirectory=` and the role's own calls via `sudo` using the same run root.
2. The sandbox is compatible with rootless podman: in particular `ProtectKernelTunables=yes` (the kernel refuses a
   fresh `/proc` mount inside a user namespace when parts of the host `/proc` are covered; if runs fail with a proc
   mount error, set it to `no`) and `ProtectSystem=strict`.
3. `Delegate=yes` makes the container's `--memory`/`--pids-limit` effective under cgroupfs (see the check above).
4. The subordinate id ranges added by the role are picked up (`podman unshare cat /proc/self/uid_map` as the user).
   If ranges are ever changed after the storage exists, run `podman system migrate` as the user.
5. The guard leaves the build (registry, npm) and runs working, DNS included, and blocks the private ranges.
6. The async build/load with `become_user` (needs `acl`), the image pruning, and the archive path.
7. `KillMode=mixed` stop behaviour during an active run (the container is gone afterwards: `podman ps -a` as the user).

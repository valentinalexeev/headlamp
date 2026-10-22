# tailscale-userspace-socks

Runs a second `tailscaled` in userspace-networking mode next to the host's normal one. It
joins the tailnet as its own node (`socks_hostname`), uses `socks_exit_node` as its exit node,
and exposes a loopback SOCKS5 port (`socks_listen`). A local proxy (for example xray) that
dials exit-bound traffic into that port makes it leave via the exit node; the host's own
traffic, routing and its primary tailscaled are untouched.

## Relation to the base role

Requires the `tailscale` role to have run first: it provides `/usr/sbin/tailscaled` and
`/usr/bin/tailscale` (the role fails early if they are missing) and `headscale_login_server`.
This role uses a separate socket, state dir and unit (`tailscaled-socks.service`).

## Variables

| Variable | Default | Notes |
|---|---|---|
| `socks_listen` | `127.0.0.1:1055` | Must match `127.0.0.1:<port>` or `[::1]:<port>` exactly (asserted). |
| `socks_hostname` | `""` | Required. Tailnet name of this second node. |
| `socks_exit_node` | `""` | Required. Node name or tailnet IP. |
| `socks_authkey` | `""` | Required until the node is logged in. Never logged. |
| `socks_user` | `tailscaled-socks` | System user/group the daemon runs as (created, no shell, no home); must not be `root`. |
| `socks_state_dir` | `/var/lib/tailscaled-socks` | Must be `/var/lib/<dir>`; becomes `StateDirectory` (mode 0700). |
| `socks_socket` | `/run/tailscaled-socks/tailscaled.sock` | Must be `/run/<dir>/<file>`; dir becomes `RuntimeDirectory` (mode 0700). |
| `socks_login_server` | `headscale_login_server` | No fallback: the caller or base role must set `headscale_login_server` (asserted). |
| `socks_verify_url` | `https://ifconfig.me` | Used by the verify tasks. |

## Behaviour

1. Preflight asserts (variables, loopback listener, binaries present).
2. Creates the `socks_user` system user/group; installs, enables and starts the unit (restarted on unit change).
3. If `status --json` reports a `BackendState` other than `Running`, runs `tailscale up --timeout=60s`:
   - first login (needs a key): `--auth-key=file:<0600 temp file>` (removed afterwards; the key
     never appears on a command line or in logs), no exit node flags, because the exit node peer
     is not yet in the netmap;
   - re-join of a logged-in node (e.g. `Stopped`): no key, and `--exit-node=...
     --exit-node-allow-lan-access=false` are restated so `up` does not refuse over existing prefs.
4. Always runs (with retries, for netmap lag) `tailscale set --exit-node=... --exit-node-allow-lan-access=false`.
5. Asserts (not optional; skipped only in `--check`) that `status --json` then reports an
   `ExitNodeStatus` and that the one peer marked `ExitNode` is `socks_exit_node` (matched by host
   name, MagicDNS name or tailnet IP). Without a selected exit node the SOCKS port would egress
   from this host, so the run fails with a message naming the likely causes (exit node offline,
   route not approved, ACL).

Verification (not run by default): `ansible-playbook ... --tags socks-verify` fetches
`socks_verify_url` through the SOCKS port and directly and prints both. The SOCKS result must
equal the exit node's egress address; the direct result must differ.

## Privileges

The daemon runs as `socks_user` (`User=`/`Group=`), with an empty `CapabilityBoundingSet=` and
`AmbientCapabilities=` (userspace networking needs none), `NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome`, `PrivateTmp`. systemd creates `StateDirectory` (`/var/lib/tailscaled-socks`) and
`RuntimeDirectory` (`/run/tailscaled-socks`) owned by that user, mode `0700`; on a host where the
unit previously ran as root, systemd chowns the existing state directory on the next start.

The Ansible tasks drive it as root: `sudo tailscale --socket=/run/tailscaled-socks/tailscaled.sock
status`. Root can open the socket inside the `0700` directory (root bypasses file permissions), and
tailscaled grants write access on its LocalAPI to uid 0 and to its own uid, so `up`/`set` work.
Other users cannot reach the socket at all. Verify on the host after the first run:

    sudo ls -ld /run/tailscaled-socks /var/lib/tailscaled-socks   # tailscaled-socks, drwx------
    ps -o user= -C tailscaled                                      # root (primary) and tailscaled-socks
    sudo tailscale --socket=/run/tailscaled-socks/tailscaled.sock status

## Security notes

- The SOCKS listener is loopback-only; the role refuses anything else. SOCKS5 here has no
  authentication, so it must never be exposed on a routable address.
- Keep `socks_authkey` in a vault. Use a pre-auth key whose ACL tag allows only what this node
  needs (the tag comes from the key, not from the client).
- The exit node applies only to traffic dialled through this SOCKS port, because the primary
  tailscaled and the host routing table are not involved.
- Assumption: that a userspace-networking tailscaled applies its selected exit node to
  connections made through its SOCKS5 server. This is verified on a real host with the
  verify tasks above, not guaranteed by this role.

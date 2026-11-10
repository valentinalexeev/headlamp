# tailscale-userspace-socks

This role runs a second `tailscaled` in userspace-networking mode next to the normal one on the host. It
joins the tailnet as its own node (`socks_hostname`) and uses `socks_exit_node` as its exit node.
It exposes a loopback SOCKS5 port (`socks_listen`). A local proxy (for example xray) can dial exit-bound traffic into that port.
This traffic then leaves through the exit node. The traffic of the host itself, its routing and its primary tailscaled do not change.

## Supported systems

The role uses `apt` and systemd. The OS matrix (`make test-ansible-os`, #170) tests it on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13. All six roles pass check mode and a real apply on these three systems, on amd64 and arm64 (#178).

For a bridge VM, use Ubuntu 26.04 Minimal. It is the preferred image. You can also use Debian 13.

## Relation to the base role

Run the `tailscale` role first. It provides `/usr/sbin/tailscaled` and
`/usr/bin/tailscale` (this role fails early if they are missing) and `headscale_login_server`.
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
| `socks_egress_check` | `true` | After step 5, one request through the SOCKS port must succeed (#259). `false` skips it. |
| `socks_egress_check_url` | `https://www.gstatic.com/generate_204` | The URL of that request. |
| `socks_verify_url` | `https://ifconfig.me` | Used by the verify tasks. |
| `socks_grant_groups` | `[]` | Tailnet grant groups (#139). Each entry has `id` (`g-<8 hex>`, from the bridge page of node-control) and `authkey` (a pre-auth key with the tag `tag:ingress-g-<8 hex>`, needed until the node is logged in). At most 8 entries. |
| `socks_grant_port_base` | `1056` | The SOCKS port of the first group. The next group uses the next port. |

## Behaviour

1. Preflight asserts: variables, loopback listener, binaries present.
2. Create the `socks_user` system user and group. Install, enable and start the unit (the role restarts it on unit change).
3. If `status --json` reports a `BackendState` other than `Running`, run `tailscale up --timeout=60s`:
   - First login (needs a key): use `--auth-key=file:<0600 temp file>`. The role removes the file afterwards.
     The key never appears on a command line or in logs. Use no exit node flags, because the exit node peer
     is not yet in the netmap.
   - Re-join of a logged-in node (for example `Stopped`): use no key. State `--exit-node=...
     --exit-node-allow-lan-access=false` again, so that `up` does not refuse because of existing prefs.
4. Always run `tailscale set --exit-node=... --exit-node-allow-lan-access=false`, with retries for netmap lag.
5. Assert that `status --json` then reports an `ExitNodeStatus`. This step is not optional. Only `--check` skips it.
   Assert also that the one peer marked `ExitNode` is `socks_exit_node`. The match is by host
   name, MagicDNS name or tailnet IP.

   Without a selected exit node, the SOCKS port would egress
   from this host. In that case, the run fails with a message that names the likely causes: exit node offline,
   route not approved, ACL.

6. With `socks_egress_check: true` (the default), request `socks_egress_check_url` through the SOCKS port. If the
   request fails, the run fails. The message names the most likely cause: no `autogroup:internet` rule for
   `tag:ingress` (see "Policy needed by a bridge").
7. For each entry of `socks_grant_groups`, install the unit `tailscaled-socks-<id>.service`. It runs one more
   userspace tailscaled with its own state dir, socket and loopback SOCKS port. On the first run, it joins with the
   key of the group as `<socks_hostname>-<id>`. It has no exit node, because it carries only tailnet traffic.
   The role stops and removes the units of groups that are no longer in the list.

   Keep the order of the list: the port of a group comes from its position. Use the same list in the
   `reality-ingress` role.

## Policy needed by a bridge

A bridge needs these rules in the headscale policy (#259). The example policy
`examples/compose/control/config/acl.json` has them, with tests:

```hujson
// the exit node, and the enrolment listener of node-control (enroll_upstream)
{"action": "accept", "src": ["tag:ingress"], "dst": ["tag:exit:*", "tag:node-control:8081"]},
// the internet through the exit node
{"action": "accept", "src": ["tag:ingress"], "dst": ["autogroup:internet:*"]},
// node-control to the bridge agent (ingress_agent_port)
{"action": "accept", "src": ["tag:node-control"], "dst": ["tag:ingress:9000"]},
```

headscale lets a node use an exit node for the internet only with `autogroup:internet` for that node. Without
the second rule:

- `tailscale set --exit-node=...` works, and the node shows the exit node as selected;
- each request through the SOCKS port fails, and this node logs `lookup <host> on [fd7a:115c:a1e0::53]:53: 403
  Forbidden`;
- the clients of the bridge connect and send, but get no reply.

Step 6 finds this fault during the run. `make test-bridge-policy-e2e` shows both cases on a real headscale.

Verification (not run by default): `ansible-playbook ... --tags socks-verify` fetches
`socks_verify_url` through the SOCKS port and directly. It prints both results. The SOCKS result must
equal the egress address of the exit node. The direct result must differ.

## Privileges

The daemon runs as `socks_user` (`User=`/`Group=`). It has an empty `CapabilityBoundingSet=` and
`AmbientCapabilities=` (userspace networking needs none). It also has `NoNewPrivileges`, `ProtectSystem=strict`,
`ProtectHome` and `PrivateTmp`.

systemd creates `StateDirectory` (`/var/lib/tailscaled-socks`) and
`RuntimeDirectory` (`/run/tailscaled-socks`), owned by that user, mode `0700`. On a host where the
unit previously ran as root, systemd changes the owner of the existing state directory on the next start.

The Ansible tasks drive it as root: `sudo tailscale --socket=/run/tailscaled-socks/tailscaled.sock
status`. Root can open the socket inside the `0700` directory, because root bypasses file permissions.
Tailscaled grants write access on its LocalAPI to uid 0 and to its own uid, so `up` and `set` work.
Other users cannot reach the socket at all. After the first run, verify on the host:

    sudo ls -ld /run/tailscaled-socks /var/lib/tailscaled-socks   # tailscaled-socks, drwx------
    ps -o user= -C tailscaled                                      # root (primary) and tailscaled-socks
    sudo tailscale --socket=/run/tailscaled-socks/tailscaled.sock status

## Security notes

- The SOCKS listener is loopback-only. The role refuses anything else. SOCKS5 here has no
  authentication, so never expose it on a routable address.
- Keep `socks_authkey` in a vault. Use a pre-auth key whose ACL tag allows only what this node
  needs. The tag comes from the key, not from the client.
- The exit node applies only to traffic dialled through this SOCKS port, because the primary
  tailscaled and the host routing table are not involved.
- Assumption: a userspace-networking tailscaled applies its selected exit node to
  connections made through its SOCKS5 server. This role does not guarantee it. Verify it on a real host with the
  verify tasks above.

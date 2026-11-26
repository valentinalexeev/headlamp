# tailguard

This role installs tailguard on a Docker host. tailguard is a WireGuard server for plain WireGuard devices, for example
a home router. The traffic of these devices leaves through a tailnet exit node. `apps/tailguard/README.md` describes
tailguard: the image, the sidecars, the watchdog and the troubleshooting table.

The role does the manual "Deploy" procedure of `apps/tailguard/README.md`:

- It installs Docker Compose and the pinned image.
- It writes `docker-compose.yml`, `.env` and `config/wg0.conf` in `tailguard_dir`.
- It makes the WireGuard server key on the host.
- It logs the tailguard node in to the tailnet with a one-time key.
- It checks the exit node and the egress through it.

The role is off by default. It runs only on the hosts that you put in the `tailguard` group of your inventory.

## Supported systems

The role uses `apt`, systemd and Docker Compose v2. Use Ubuntu 26.04 (Server or Minimal) or Debian 13. The OS matrix
(`make test-ansible-os ROLES="tailscale tailguard"`) runs the role on these systems in check mode and with a real apply.
It also runs a second apply, and makes sure that the second apply changes nothing.

When `docker compose version` fails, the role installs Docker from the packages of the distribution:

| System | Packages (`tailguard_docker_packages`) |
|---|---|
| Ubuntu | `docker.io`, `docker-compose-v2` |
| Debian | `docker.io`, `docker-compose` |

If the host has Docker Engine from the Docker repository, the role does not change it. To stop the install, set
`tailguard_install_docker: false`. The role then stops when Docker Compose is missing.

## Before you start

1. In `tagOwners` of the tailnet policy, give `tag:tailguard` only to the admin or service identities.
2. In the tailnet policy, give `tag:tailguard` the internet through the exit node and no tailnet destinations:
   `{"action": "accept", "src": ["tag:tailguard"], "dst": ["autogroup:internet:*"]}`.
3. Make sure that the exit node has `tag:exit` (or the tag in `tailguard_exit_node_tag`).
4. Make a one-time (not reusable) pre-auth key with the tag `tag:tailguard`.
5. Put the key in the vault of your site repository as `tailguard_authkey`.

**WARNING:** Do not use an untagged key that your user owns. The node then gets all the access of that user, and each
WireGuard peer can reach the same tailnet addresses.

## Variables

These variables have no default. The role stops with a message that names the variable when one is missing.

| Variable | Value |
|---|---|
| `tailguard_hostname` | The tailnet name of the tailguard node, for example `tg-node`. |
| `tailguard_authkey` | The one-time key with `tag:tailguard`. It is necessary only until the node is logged in. Keep it in a vault. |
| `tailguard_exit_node_prefix` | The name prefix of the exit node. `my-exit` also selects `my-exit-1`. |
| `tailguard_lan_access` | `"0"` (recommended): the peers reach the internet through the exit node only. `"1"`: the peers can also reach the LAN of the host. |
| `tailguard_bind_addr` | `<host-lan-ip>` (recommended): the WireGuard and tailscale ports listen on that host address only. `0.0.0.0`: the ports listen on all host addresses. |
| `headscale_login_server` | The URL of headscale. The role uses it as `tailguard_login_server`. |

These variables have a default.

| Variable | Default | Value |
|---|---|---|
| `tailguard_image` | `ghcr.io/valentinalexeev/headlamp/tailguard:0.22.0` | A release `X.Y.Z` or a digest `@sha256:...`. The role refuses `latest`, `main` and an image without a tag. |
| `tailguard_image_archive` | `""` | A `docker save` archive of `tailguard_image` on the control machine. The role loads it and does not pull. |
| `tailguard_dir` | `/opt/tailguard` | The directory on the host. Only root can read it. |
| `tailguard_install_docker` | `true` | Install Docker when `docker compose version` fails. |
| `tailguard_ts_port` | `41642` | The UDP port of tailscaled in the container. The tailscaled of the host uses 41641. |
| `tailguard_exit_node_tag` | `tag:exit` | The tracker selects only exit nodes with this tag. |
| `tailguard_block_derp_regions` | `""` | DERP region codes that tailscaled must not use as home. See "exit-node-tracker" in `apps/tailguard/README.md`. |
| `tailguard_status_port` | `""` | Empty (recommended): the status page is off. A port, for example `8088`: the status page is on, on `wg0` and `tailscale0`. |
| `tailguard_nameservers` | `""` | The DNS servers of the container (`TG_NAMESERVERS`). Empty: the default of the image. |
| `tailguard_wg_port` | `51820` | The UDP port of the WireGuard server. |
| `tailguard_wg_address` | `["10.99.0.1/24", "fd99:7461:6c67::1/64"]` | The addresses of `wg0`. |
| `tailguard_peers` | `[]` | The WireGuard peers. See "Peers". |
| `tailguard_ipv6_subnet` | `fd99:7461:6c68::/64` | The IPv6 subnet of the compose network. Empty: IPv4 only. |
| `tailguard_exit_node_wait` | `180` | The time in seconds for the tracker to select the exit node. |
| `tailguard_egress_check` | `true` | Send one request through the exit node after the start. `false` skips it. |
| `tailguard_egress_check_url` | `https://www.gstatic.com/generate_204` | The URL of that request. |

## Peers

Each entry of `tailguard_peers` is one WireGuard peer:

```yaml
tailguard_peers:
  - name: router1                                         # a label in wg0.conf only
    public_key: "<public key of the peer>"
    allowed_ips: ["10.99.0.2/32", "fd99:7461:6c67::2/128"]
    preshared_key: "{{ vault_tailguard_psk_router1 }}"   # optional
```

- The peer makes its own key pair. Give only its public key to the role.
- A preshared key is a secret. Keep it in a vault. Make one with `wg genpsk`.
- The role writes the peers to `config/wg0.conf` (mode `0600`). The task that writes the file has no log and no diff.
- When you change the list, the role starts the containers again.

The list is a variable until node-control manages the peers (#393).

## What the role does

1. Check the variables. The role does not show the values of keys in a message.
2. If `docker compose version` fails, install the Docker packages. Start Docker.
3. Make `tailguard_dir` with `config/`, `state/` and `run/` (owner root, mode `0700`).
4. Pull `tailguard_image`, or load `tailguard_image_archive`, if the image is not on the host.
5. If `config/server.key` does not exist, make the WireGuard server key in a container without network.
   The role never reads the private key. `wg0.conf` loads it with `PostUp = wg set %i private-key ...`.
   The role keeps the public key in `config/server.pub` and shows it at the end of the run.
6. Write `docker-compose.yml`, `.env` and `config/wg0.conf`.
7. If `state/` is empty, log in (see "Login").
8. If a file changed, create the containers again. If a service does not run, start it.
9. If the node needs a login again (for example, an admin deleted it), log in (see "Login").
10. Wait until the tracker selects an exit node, for not more than `tailguard_exit_node_wait` seconds.
    Make sure that the selected node is online, has `tailguard_exit_node_tag` and has a name with
    `tailguard_exit_node_prefix`. If not, the run fails.
11. With `tailguard_egress_check: true`, send one request from the container through the exit node. If it fails, the
    run fails. The message gives the most likely cause: the policy has no `autogroup:internet` rule for `tag:tailguard`.
12. Show the WireGuard server public key. Use it as the `PublicKey` of the server in the configuration of each peer.

## Login

The role uses `tailguard_authkey` only when the node is not logged in. It does these steps:

1. Write the key to a new file `run/authkey.<random>`, with mode `0600`.
2. Write `TS_AUTHKEY=file:/tmp/authkey.<random>` in `.env`. The container mounts `run/` at `/tmp`.
3. Create the containers again, and wait until the node is `Running`.
4. Remove the key file and the `TS_AUTHKEY` line, also when the login fails.
5. Create the containers again without the key.

The key is never on a command line, in `.env`, in a container setting or in the log of the run. `ps` shows only the
path of the key file.

## Check mode

`--check` shows the changes to the packages, the directories and the files. It does not make a key, start a
container or log in. The checks of the exit node and the egress run only in a real apply.

## The tests

- `make test-ansible-roles` runs `tests/render.sh`. It checks the variables and the files that the role writes,
  without Docker.
- `make test-ansible-os ROLES="tailscale tailguard"` runs the role on the OS matrix with a real headscale. The
  `tailscale` role makes the exit node. The run checks that the three services run and that a second apply changes
  nothing. It also checks that the join key is not in the logs, in the files or in the container settings.

## Limits

- The role starts the tailguard containers with Docker Compose. It does not configure the router of the peers. See
  "Home-router client config" in `apps/tailguard/README.md`.
- The role does not remove tailguard. To remove it, run `docker compose down` in `tailguard_dir`, then remove the
  directory and the node in headscale.

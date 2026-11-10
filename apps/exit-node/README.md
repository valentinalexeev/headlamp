# apps/exit-node — tailscale exit node image

A container that joins your tailnet (headscale or Tailscale) as an exit node. It advertises
extra `/32` routes for the hostnames in `direct-hosts.txt`. The script `update-direct-routes.sh` resolves these hostnames again every 3 hours.
Those services use this node, whichever exit node a client selected. This app is derived from patte/fly-tailscale-exit. See `THIRD_PARTY.md`.

The image runs on any container host that gives it what a tailnet router needs. Fly.io is one option, and this
directory has a `fly.toml` for it. For an exit node on a VM, use `terraform/modules/exit-node-vm` and the Ansible
roles instead. They do not use this image.

## Requirements of the host

- Linux with `NET_ADMIN`, access to `/dev/net/tun`, and IPv4 and IPv6 forwarding on. At start, `start.sh` loads
  `xt_mark`, tries to turn forwarding on and adds a `MASQUERADE` rule on `eth0`. The interface name `eth0` is fixed.
  A container usually cannot change sysctls itself (the log says `Read-only file system`), so set forwarding from
  the host. Without it, the node joins but forwards nothing.
- Inbound UDP on port `41641` (optional, for direct connections). Without it, clients reach the node through DERP.
- A persistent directory at `/var/lib/tailscale` for the node state, or Redis state mode (see below). Without
  either, every restart registers a new node.

## Configuration

| Variable | Kind | Meaning |
| --- | --- | --- |
| `TAILSCALE_AUTH_KEY` | secret | A reusable, non-ephemeral auth key. Ephemeral keys are IPv6-only on Tailscale and work poorly for exit nodes. |
| `HEADSCALE_LOGIN_SERVER` | env | `https://<your-headscale-url>` |
| `EXIT_NODE_HOSTNAME` | env | The tailnet name of the node. The default is `ams`. |
| `DIRECT_HOSTS_FILE` | env | Path of a mounted host list that replaces the built-in placeholder (see `docs/images.md`). |
| `HEALTH_PORT` | env | The port of the health endpoint (#196). The default is `9002`. `off` turns it off. |
| `HEALTH_EGRESS_URL` | env | The URL of the internet check of the health endpoint. The default is `https://www.gstatic.com/generate_204`. |

- `direct-hosts.txt` here is a placeholder. Keep the real one in your private repo. Mount it and set
  `DIRECT_HOSTS_FILE`, or copy it over before you build (see `CLAUDE.md`, "overlay procedure").
- The node advertises itself as an exit node. Approve it (or use the ACL `autoApprovers.exitNode`) before clients
  can use it.

## Health (#196)

`GET :9002/` answers `200` when all of these are true, else `503` with the failed checks:

- tailscaled is `Running`;
- the node has the exit route `0.0.0.0/0`, approved in headscale;
- IPv4 forwarding is on, and the `MASQUERADE` rule on `eth0` is in place;
- this node reaches `HEALTH_EGRESS_URL` (the uplink of the exit traffic). The result is kept for 60 seconds.

The `[checks]` section of `fly.toml` uses it. `fly checks list` shows the result.

The Fly kernel has no `xt_connmark`. tailscale 1.104 tries to add connmark rules for `rp_filter`, so it logs
`router config failed (enabling connmark rules ...)` at every start. This is expected on Fly: the NAT and filter
rules are in place, and exit traffic flows. `start.sh` logs one `info:` line about it, and the health endpoint shows
that forwarding works.

## Run on Fly.io

`fly.toml` here is a placeholder. Keep the real one in your private repo.

- State: `flyctl volumes create tailscale_state -a <app> -r <region> -n 1`. The `[mounts]` section of `fly.toml`
  mounts it at `/var/lib/tailscale`.
- Secrets: `flyctl secrets set TAILSCALE_AUTH_KEY=<key> HEADSCALE_LOGIN_SERVER=https://<your-headscale-url>`.
- Deploy: `flyctl deploy [--build-arg TSVERSION=<x.y.z>]`.

## Run with Docker or another container host

```sh
docker run -d --name exit-node --cap-add NET_ADMIN --device /dev/net/tun -p 41641:41641/udp \
  --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv6.conf.all.forwarding=1 \
  -v exit-node-state:/var/lib/tailscale \
  -e TAILSCALE_AUTH_KEY=<key> -e HEADSCALE_LOGIN_SERVER=https://<your-headscale-url> \
  ghcr.io/valentinalexeev/headlamp/exit-node:<tag>
```

Give the container an `eth0` that leads to the internet (the default Docker bridge does). On Kubernetes, the same
capability, device, volume and sysctls apply. The forwarding sysctls are unsafe sysctls there, so the kubelet must
allow them. Checked with Docker: forwarding is on, the `MASQUERADE` rule is in place and tailscaled starts.

## Redis state mode (optional)

Instead of the volume, the node can keep its tailscaled state file in Redis,
encrypted with age. `state-sync.sh restore` runs before tailscaled starts. `state-sync.sh watch`
pushes the file when its checksum changes. A final `push` runs on stop. When `REDIS_URL`
is unset, everything below is off and the node works as described above.

| Variable | Kind | Default | Meaning |
| --- | --- | --- | --- |
| `REDIS_URL` | secret | unset (sync off) | Redis connection URL |
| `STATE_AGE_IDENTITY` | secret | none | age identity (`AGE-SECRET-KEY-...`); the state is encrypted to its recipient |
| `STATE_REDIS_KEY` | env | `exit-node:tailscaled-state` | Redis key holding the encrypted state |
| `STATE_FILE` | env | `/var/lib/tailscale/tailscaled.state` | local state file |
| `STATE_POLL_SECONDS` | env | `60` | how often the watcher checks the file |
| `BOOT_WAIT_SECONDS` | env | `60` | wall-clock seconds `restore` waits for Redis at boot |
| `REDIS_TIMEOUT_SECONDS` | env | `5` | per `redis-cli` call limit (uses `timeout`, present in the image); keeps a hung connection from stalling boot or the shutdown push |

- Secrets: set `REDIS_URL` and `STATE_AGE_IDENTITY` as secrets of your platform (on Fly.io:
  `flyctl secrets set REDIS_URL=<redis url> STATE_AGE_IDENTITY=<age secret key>`).
  Generate the identity with `age-keygen`. Keep a copy outside the app. Without the copy, you cannot decrypt the
  stored state. The node never logs these values.
- Key naming: use one Redis key for each node. If several nodes share
  a Redis instance, give each node its own `STATE_REDIS_KEY`.
- Boot: `restore` waits up to `BOOT_WAIT_SECONDS` (default 60 s) for Redis. If Redis is still down
  and there is no local state file, startup exits 1. The machine then restarts and tries again. If a local
  file exists, the node uses it. A decrypt failure, a missing or invalid `STATE_AGE_IDENTITY`, or a
  malformed Redis reply also stops startup.
- Divergence after an outage: the first watcher cycle after boot pushes the current local file. A
  local file that diverged while Redis was down overwrites Redis when Redis is back. This is intended.
  After a fallback, the local state of the node is the newest.
- The volume is optional when Redis mode works. After a restore from Redis works on a
  redeploy, you can remove the `[mounts]` section. Keep it until then as a fallback.

# apps/fly/exit-ams — Fly.io tailscale exit node

A Fly machine that joins your tailnet (headscale or Tailscale) as an exit node. It advertises
extra `/32` routes for the hostnames in `direct-hosts.txt`. The script `update-direct-routes.sh` resolves these hostnames again every 3 hours.
Those services use this node, whichever exit node a client selected. This app is derived from patte/fly-tailscale-exit. See `THIRD_PARTY.md`.

- `fly.toml` and `direct-hosts.txt` here are placeholders. Keep the real ones in your private
  repo. Copy them over before you deploy (see `CLAUDE.md`, "overlay procedure").
- State: `flyctl volumes create tailscale_state -a <app> -r <region> -n 1`. With the volume, the node keeps its
  identity across deploys. Without the volume, every deploy registers a new node.
- Secrets: `flyctl secrets set TAILSCALE_AUTH_KEY=<reusable, non-ephemeral key>` and
  `HEADSCALE_LOGIN_SERVER=https://<your-headscale-url>`. Use a reusable non-ephemeral key.
  Ephemeral keys are IPv6-only on Tailscale and work poorly for exit nodes.
- Deploy: `flyctl deploy [--build-arg TSVERSION=<x.y.z>]`. The node advertises itself as an exit
  node. Approve it (or use the ACL `autoApprovers.exitNode`) before clients can use it.

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

- Secrets: `flyctl secrets set REDIS_URL=<redis url> STATE_AGE_IDENTITY=<age secret key>`.
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

# apps/fly/exit-ams — Fly.io tailscale exit node

A Fly machine that joins your tailnet (headscale or Tailscale) as an exit node and advertises
extra `/32` routes for the hostnames in `direct-hosts.txt` (re-resolved every 3 hours by
`update-direct-routes.sh`), so those services route through this node regardless of which exit
node a client selected. Derived from patte/fly-tailscale-exit, see `THIRD_PARTY.md`.

- `fly.toml` and `direct-hosts.txt` here are placeholders; keep the real ones in your private
  repo and copy them over before deploying (see `CLAUDE.md`, "overlay procedure").
- State: `flyctl volumes create tailscale_state -a <app> -r <region> -n 1` (the node keeps its
  identity across deploys; without it every deploy registers a brand-new node).
- Secrets: `flyctl secrets set TAILSCALE_AUTH_KEY=<reusable, non-ephemeral key>` and
  `HEADSCALE_LOGIN_SERVER=https://<your-headscale-url>`. Use a reusable non-ephemeral key
  (ephemeral keys are IPv6-only on Tailscale and work poorly for exit nodes).
- Deploy: `flyctl deploy [--build-arg TSVERSION=<x.y.z>]`. The node advertises itself as an exit
  node; approve it (or use the ACL `autoApprovers.exitNode`) before clients can use it.

## Redis state mode (optional)

Instead of relying on the volume, the node can keep its tailscaled state file in Redis,
encrypted with age. `state-sync.sh restore` runs before tailscaled starts, `state-sync.sh watch`
pushes the file whenever its checksum changes, and a final `push` runs on stop. With `REDIS_URL`
unset everything below is off and the node behaves exactly as described above.

| Variable | Kind | Default | Meaning |
| --- | --- | --- | --- |
| `REDIS_URL` | secret | unset (sync off) | Redis connection URL |
| `STATE_AGE_IDENTITY` | secret | none | age identity (`AGE-SECRET-KEY-...`); the state is encrypted to its recipient |
| `STATE_REDIS_KEY` | env | `exit-node:tailscaled-state` | Redis key holding the encrypted state |
| `STATE_FILE` | env | `/var/lib/tailscale/tailscaled.state` | local state file |
| `STATE_POLL_SECONDS` | env | `60` | how often the watcher checks the file |
| `BOOT_WAIT_SECONDS` | env | `60` | how long `restore` waits for Redis at boot |

- Secrets: `flyctl secrets set REDIS_URL=<redis url> STATE_AGE_IDENTITY=<age secret key>`.
  Generate the identity with `age-keygen`; keep a copy outside the app, since without it the
  stored state cannot be decrypted. Values are never logged.
- Key naming: one Redis key per node. Give each node its own `STATE_REDIS_KEY` if several share
  a Redis instance.
- Boot: `restore` waits up to 60 s for Redis. If Redis is still down and there is no local state
  file, startup exits 1 (the machine restarts and retries). If a local file exists, it is used.
- Divergence after an outage: when Redis was unreachable and the node booted from its local
  file, a local file that differs from what Redis holds is not pushed until the file changes
  again. The watcher only pushes on change.
- The volume is optional once Redis mode is proven: after a restore from Redis has worked on a
  redeploy, the `[mounts]` section can be dropped. Keep it until then as a fallback.

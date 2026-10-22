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

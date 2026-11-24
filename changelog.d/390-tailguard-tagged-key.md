---
issue: 390
type: fix
section: Security
status: done
breaking: true
title: tailguard joins as a tagged node with internet-only access
components: [tailguard]
---
## Summary

tailguard joins the tailnet with a one-time pre-auth key for `tag:tailguard` (new in `tagOwners` of the example
policies), not as a user. The example policy gives the tag only `autogroup:internet`, so a WireGuard peer reaches no
tailnet address. The exit-node tracker selects only exit nodes with `EXIT_NODE_TAG` (default `tag:exit`). Three
settings are now required in `.env` and `docker compose up` stops without them: `TG_LAN_ACCESS` (`0` internet only,
`1` the LAN of the host too), `TG_STATUS_PORT` (empty = status page off; it replaces `TG_WEBUI_PORT`) and
`TG_BIND_ADDR` (the host address of the published ports; `0.0.0.0` = all). See `apps/tailguard/README.md`.

## Upgrade

tailguard (`apps/tailguard`), if you run it:

1. Add `tag:tailguard` to `tagOwners` of your policy (admin and service identities only) and the rule
   `{"action": "accept", "src": ["tag:tailguard"], "dst": ["autogroup:internet:*"]}`.
2. Add to `.env`: `TG_LAN_ACCESS=0`, `TG_STATUS_PORT=` and `TG_BIND_ADDR=<host-lan-ip>`. To keep the old behavior,
   set `TG_LAN_ACCESS=1`, `TG_STATUS_PORT=8088` and `TG_BIND_ADDR=0.0.0.0`. Remove `TG_WEBUI_PORT`.
3. Make sure that your exit nodes have `tag:exit`, or set `EXIT_NODE_TAG`.
4. Register the node again with the tagged key: put a one-time `tag:tailguard` pre-auth key in `TS_AUTHKEY`, run
   `docker compose down && rm -rf state/ && docker compose up -d`, then delete the old node in headscale and remove
   `TS_AUTHKEY` from `.env`.

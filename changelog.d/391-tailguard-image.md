---
issue: 391
type: feature
section: Added
status: done
breaking: false
title: tailguard image with tailscale at the repo pin
components: [tailguard]
---
## Summary

New image `ghcr.io/valentinalexeev/headlamp/tailguard`: the upstream tailguard image at a pinned digest, with
tailscale at `TAILSCALE_VERSION` and the sidecar scripts `route-health.sh`, `track-exit-node.sh` and `verify.sh` in
`/usr/local/bin`. `apps/tailguard/docker-compose.yml` runs all three services from it at a release version, not
`ghcr.io/juhovh/tailguard:latest`, and no longer bind-mounts the scripts. The tailscale pin check covers the image.

## Upgrade

tailguard (`apps/tailguard`), if you run it:

1. Copy the new `docker-compose.yml` (or change the three `image:` lines to
   `ghcr.io/valentinalexeev/headlamp/tailguard:<release>` and remove the script bind mounts and entrypoint paths
   as in the new file).
2. Run `docker compose up -d`. The state in `state/` stays; the node does not log in again.

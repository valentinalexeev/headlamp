---
issue: 434
type: task
section: Changed
status: done
title: Tailscale pin 1.104.7
components: [exit-node, node-control, funnel-ingress, tailnet-connector, ansible]
---
## Summary

The tailscale pin `TAILSCALE_VERSION` is now `1.104.7` (was `1.104.5`). The exit-node, node-control, funnel-ingress,
funnel-sidecar and tailnet-connector images, the `tailscale.com` Go module, the Ansible `tailscale` role default and
the `tailnet-connector` chart `appVersion` use this version.

## Upgrade

Set the site `TAILSCALE_VERSION` file to `1.104.7` and run `make bump-tailscale-version VERSION=1.104.7`.

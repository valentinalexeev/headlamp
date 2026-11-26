---
issue: 401
type: feature
section: Added
status: done
breaking: false
title: Expire a tailnet node from the node-control console
components: [node-control]
---
## Summary

node-control: the Nodes page has a section "Tailnet nodes" for nodes admins. **Expire node** asks for confirmation,
shows what stops, expires the node in headscale and also expires the reusable pre-auth key it registered with, if that
key is still valid. The node can then join again only with a new one-time key. The audit log records the admin, the
node and the time (`node.expire`). New API: `GET /nodes/tailnet.json` and `POST /nodes/tailnet/{hsid}/expire` (nodes
admin level; a tagged caller gets 403). See "Expire a node" in `apps/node-control/README.md`.

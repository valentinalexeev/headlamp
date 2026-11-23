---
issue: 449
type: docs
section: Added
status: done
title: A checklist for the admin console on HTTPS
components: [node-control, docs]
pr: 453
---
## Summary

The node-control README has "Admin console on HTTPS: checklist": the policy rule for `tag:node-control:443` and
`tag:node-control:53`, split DNS, the IdP client, the first sign-in and a troubleshooting table. `docs/dns.md` tells
that the people who use the serve names need port 53 of node-control. The example policy of `examples/compose/control`
has the console rule for `admin@` and its policy tests.

## Upgrade

The admins need `tag:node-control:53` next to `tag:node-control:443` in the policy. If the console name times out on
the devices, add the rule of "Admin console on HTTPS: checklist" in the node-control README.

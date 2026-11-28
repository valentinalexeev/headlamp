---
issue: 461
type: feature
section: Added
status: done
breaking: false
title: Tailnet grants to group:<name> resolve from the OIDC groups of the owner
components: [node-control]
---
## Summary

A tailnet grant to `group:<name>` on a bridge in `"tailnet_access": "grants"` mode can now give access. Set
`OIDC_GROUP_GRANT_HOURS` (1 to 720, more than `OIDC_RECHECK_MINUTES`): node-control then stores the groups claim of
each OIDC sign-in on `/me` (only the `OIDC_ALLOWED_GROUPS` values when that is set), refreshes it at each re-check,
and matches group grants against it. The groups stop counting after N hours without a sign-in or re-check, after a
sign-out on `/me`, and when the person is locked. The bridge page shows each group grant with its number of matching
devices. The default is 0: off, and group grants stay "not resolved", as before. See "Group grants" in the
node-control README.

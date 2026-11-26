---
issue: 469
type: feature
section: Added
status: done
breaking: false
title: node-control asks for a new sign-in before sensitive console operations
components: [node-control]
---
## Summary

node-control: with `ADMIN_OIDC_REAUTH=1` (off by default, needs `ADMIN_OIDC_REQUIRED=1`), a sensitive route of the
console on HTTPS (key, grant, role, secret and policy changes) needs a sign-in of the last `ADMIN_OIDC_REAUTH_MINUTES`
(default 3). An older session gets a new sign-in with `prompt=login` and `max_age`. An identity provider that does not
prove the new sign-in with `auth_time` is refused and the session ends (`ADMIN_OIDC_REAUTH_UNPROVEN=logout`, the
default), or accepted with a warning in the log (`warn`). `ADMIN_OIDC_REAUTH_ROUTES` adds or removes routes. See
"Re-authentication for sensitive operations" in `apps/node-control/README.md`.

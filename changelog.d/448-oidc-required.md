---
issue: 448
type: feature
section: Changed
status: done
breaking: true
title: The console on HTTPS needs the OIDC sign-in as a second factor
components: [node-control]
---
## Summary

With `ADMIN_OIDC=1`, the console on HTTPS now needs the OIDC sign-in on each request, in addition to the tailnet login
(`ADMIN_OIDC_REQUIRED`, default `1`). Without a session, a page load goes to the sign-in and comes back to the page.
An API call gets `401`.

A session ends after `ADMIN_OIDC_IDLE_MINUTES` without a request (default 30). The top bar
shows "Sign in" or "Signed in as" and "Sign out". The plain listener on port 80 does not change: it is the break-glass
way in, with one factor. See "Required sign-in" in `apps/node-control/README.md`.

## Upgrade

If you set `ADMIN_OIDC=1`: after the upgrade, each admin must sign in on `https://<ADMIN_CONSOLE_NAME>`. Without a
session, the logins of `ADMIN_USERS` have no role on port 443. Make sure that the console client at the identity
provider works for each admin before you upgrade. To keep the old behavior, set `ADMIN_OIDC_REQUIRED=0`. Check who can
reach port 80 of node-control in the tailnet policy: that listener still accepts the tailnet login alone.

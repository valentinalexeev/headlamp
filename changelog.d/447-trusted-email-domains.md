---
issue: 447
type: feature
section: Added
status: done
title: Trusted email domains for the admin sign-in
components: [node-control]
---
## Summary

`ADMIN_OIDC_TRUSTED_EMAIL_DOMAINS` (off by default) lets an email in a listed domain count as verified for the admin
console sign-in when `email_verified` is false or missing. The email must still be the tailnet login, and the startup
log names the domains. The node-control README, "Identity providers without `email_verified`", tells how to send the
claim from Authentik, Keycloak and Entra ID, and when a trusted domain is safe.

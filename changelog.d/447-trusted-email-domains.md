---
issue: 447
type: feature
section: Added
status: done
title: Trusted email domains for the admin sign-in
components: [node-control]
---
## Summary

`ADMIN_OIDC_TRUSTED_EMAIL_DOMAINS` (off by default) lets the admin sign-in accept an email in a listed domain when the
identity provider sends `email_verified` false or no claim. The email must still be the tailnet login. See "Trusted
email domains" in the node-control README, also for the Authentik, Keycloak and Entra ID mappings.

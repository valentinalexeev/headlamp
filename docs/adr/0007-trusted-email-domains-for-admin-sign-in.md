# ADR 0007: Trusted email domains for the admin sign-in

Status: accepted. The owner chose this option on #447 ("As proposed").

## Context

The admin sign-in (#366, #446) compares the email of the ID token with the tailnet login. It accepts the email only
when the ID token has `email_verified=true`. Some identity providers send `false` or no claim, for example Authentik
with an external source, or Microsoft Entra ID, which has no such claim. An admin of such a provider cannot sign in.

## Decision

1. A new setting, `ADMIN_OIDC_TRUSTED_EMAIL_DOMAINS`, a comma list of email domains. It is off by default. When it is
   set, node-control logs each domain at start.
2. A trusted domain applies only when `email_verified` is false or missing. The email must still equal the tailnet
   login. A token with a different email is refused with `other_account`, also when that email is verified.
3. The domain is the part after the last `@`. The match is exact and does not use case. There is no subdomain
   wildcard.
4. The README gives the mappings for Authentik, Keycloak and Entra ID. An operator first fixes the claim there. The
   README also has a warning. A domain is safe only when nobody can sign in with an address there without the
   mailbox.

## Rejected options

- **Docs only (option 2 alone).** It does not help when the operator cannot change the identity provider, for
  example with Entra ID.
- **Another claim instead of the email (option 3).** It changes what the match means: the console would no longer
  compare the email with the tailnet login. No real identity provider needs it now.

## Consequences

- An operator who trusts a domain also trusts the identity provider to control the addresses of that domain.
- Tests: `TestCheckTrustedDomains`, `TestTrustedDomain` and `TestParseDomains` in
  `apps/node-control/internal/adminoidc/trusted_test.go`.

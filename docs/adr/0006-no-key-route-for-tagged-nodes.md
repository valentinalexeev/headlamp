# ADR 0006: No route that a tagged node reaches mints or returns a key

Status: accepted. A technical rule from the key management review (#403). The owner accepted the plan on #395.

## Context

node-control mints headscale pre-auth keys in three places. All three are for people:

- serve: `POST /serve/services` and `POST /serve/services/{id}/keys` (a serve admin);
- funnel: `POST /funnel/namespaces/{name}/keys` (a funnel admin);
- self-enrolment: `POST /me/devices` on the public listener (a person with an OIDC session and the CSRF value of the
  form).

A tagged node is, for example, an exit node, a bridge, a funnel ingress or a serve proxy. It reaches only the machine
routes of its own tag: `GET /ingress/v1/config`, `POST /ingress/v1/state` (`tag:funnel-ingress`) and
`POST /serve/v1/cert` (`tag:serve`). None of these mints, returns or forwards a key.

This rule was not written down, and no test checked it. A design that sends a key to a node through node-control
breaks it. An example is a key rotation through a pull path. Then an attacker who controls a node receives the key
and registers a second node on another machine. That node stays after the operator cleans or removes the first node.
One-time keys and short expiry do not help, because the attacker gets the key itself.

## Decision

1. **No route for a tagged node mints, returns or forwards a pre-auth key, an API key or an admin credential.** A
   route that does is not a machine route (`KindMachine`), and it is not open (`KindOpen`).
2. **A key reaches a node only through the channel of the operator.** This is Ansible over SSH, or the operator at
   the host (a secret store, a file with mode 0600). A node never asks node-control for a key.
3. A design that needs a key on a node without the operator gives the reason in an ADR first. Examples are a key
   rotation, an automatic recovery or the per-identity nodes of #383. That ADR must show why the rule does not
   apply. Or it shows how a node under attack cannot get a key that works on another machine.

## Check

`TestNoKeyRouteForTaggedNodes` (`apps/node-control/cmd/console-demo/keyroutes_test.go`) runs every module with the
production Authorizer:

- It calls every admin route as a tagged node, for each machine tag. Each route answers 403, except an open route and
  the machine routes of the caller's own tag.
- Each machine route must be on the list in the test. A new machine route thus fails the test until a person checks
  it against this ADR and adds it.
- No answer of any admin or public route has the text of a key, and the fake headscale mints no key during the walk.

`core.Mux.Seal` already refuses a route without a rule. This test makes sure that the rule is safe for keys.

## Consequences

- A person who adds a machine route also updates the list in the test. The list shows each route that a tagged node
  can reach.
- The design of #383 (a tailnet node for each OIDC identity) must answer rule 3 before it adds a key path.

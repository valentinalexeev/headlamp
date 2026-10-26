# funnel-ingress

Publishes services of tailnet nodes on the public internet under stable names, the way Tailscale Funnel does,
for a self-hosted headscale network. headscale does not implement Funnel (no `funnel` node attribute, no
control-issued certificates, no relay fleet), so `tailscale funnel` on a client cannot work here. This app is
the equivalent: a small public reverse proxy on its own Fly app, joined to the tailnet as one more node.

```
Internet -> Fly edge (TLS, wildcard certs) -> funnel-ingress machine (Go, embedded tailscale node)
                                               |- public listener  :8080  reverse proxy, routed by Host
                                               |- tailnet listener :9100  registration API (not on the internet)
                                               |- reconcile loop          peer list vs. registered routes
                                               '- sync loop              pulls config from node-control, reports state
                                               v dials over the tailnet
                                     target node (tag:funnel + tag:ns-<ns>) :port
```

A service `admin` in namespace `pdl` is served at `https://admin.pdl.<public domain>/`. One wildcard record and one
wildcard certificate per namespace; adding a service afterwards touches no DNS and no certificate.

**Single admin surface:** the ingress has no admin API, page, tokens or users. Namespaces, their allowed ports,
manual routes and blocked names are managed in node-control and pulled by the ingress; node-control never calls
the ingress. Without node-control, a local JSON file of the same shape works (`FUNNEL_CONFIG_FILE`).

## Routing

Host `<service>.<namespace>.<public domain>` (exactly two labels before the domain; case, a port and one trailing
dot are normalised) maps to a route; anything else is a bare `404`. The original `Host` is forwarded;
`X-Forwarded-For/Host/Proto` are rebuilt from scratch (client-supplied forwarding headers are dropped; the client
address is the `Fly-Client-IP` set by Fly's edge); hop-by-hop headers are stripped; WebSocket upgrades work;
connect, idle and response-header timeouts and a request-body limit apply. A known route whose backend is unreachable
answers `503` (`502` for a broken backend), never with internal names or addresses.

Backends are `http` by default (the tailnet is already encrypted). A static route may use `"scheme": "https"`, which
verifies the certificate against the target address, and optionally `"insecure_skip_verify": true`.

Two sources feed one route table, with fixed precedence:

- **static routes** from the configuration source; read-only to the API and they always win (a registration for the
  same name gets `409`);
- **dynamic routes** registered by nodes, persisted in the state store.
- **blocked names** from the configuration: registration is refused (`403`) and an existing dynamic route is removed.

## Registration API (tailnet only, default port 9100)

The caller's identity comes from the control plane (`WhoIs` on the connecting tailnet address), never from the
request. A node may publish only when **all** hold:

1. it carries the funnel tag (`FUNNEL_TAG`, default `tag:funnel`);
2. it carries a namespace tag (`FUNNEL_NS_TAG_PREFIX`, default `tag:ns-`, so `tag:ns-pdl` is namespace `pdl`); the
   namespaces it may publish are exactly those tags (with several, the request must name one);
3. the port is inside the namespace's allowed ranges (an empty list rejects everything).

The backend of a route is always the **caller's own tailnet address**; a body cannot name another one.

| call | meaning |
|---|---|
| `PUT /v1/routes/{service}` body `{"ns":"pdl","port":8080,"scheme":"http"}` | create or update (idempotent for the same node) |
| `DELETE /v1/routes/{service}?ns=pdl` | withdraw (owner only) |
| `GET /v1/routes` | the caller's own routes |

Answers carry no body: `204`/`200`, `401` (unknown caller), `403` (tags, namespace, port, blocked name), `409` (owned
by another node, or a static route), `422` (bad input), `429` (per-node route limit), `503` (could not be persisted:
refused, nothing changed). Ports of the ingress itself (proxy, API) cannot be published, and the proxy node cannot
register itself.

Helper for a node: `register-service.sh [--scheme https] SERVICE PORT [NAMESPACE]` and `register-service.sh --delete
SERVICE [NAMESPACE]` (needs `FUNNEL_API_HOST`; idempotent, run it at boot or from a timer).

## Lifecycle

A loop (`FUNNEL_RECONCILE_INTERVAL`, default 60 s) compares dynamic routes with the proxy node's peer list:

- owner **present but offline**: the route stays (requests get `503`); there is no heartbeat expiry;
- owner **absent for `FUNNEL_MISSING_CHECKS` consecutive checks** (default 3), or **no longer carrying the funnel tag or
  that namespace tag**: all its routes are removed and each is logged (`node, ns, service`);
- routes are bound to the **stable node ID**, so a deleted and re-enrolled node with the same name inherits nothing;
- after a start nothing is cleaned up until the first complete peer list is loaded.

The peer list is filtered by the headscale ACL: if an ACL change hides a node from the proxy, its routes are removed.
That is intended, since the proxy could not reach it anyway.

## Environment

Secrets go in Fly secrets; plain settings in `fly.toml`. Namespaces, ports, the public domain and routes are **not**
process settings; they arrive through the configuration source.

| variable | purpose |
|---|---|
| `TS_AUTHKEY` | secret; pre-auth key for the proxy node, tagged `tag:funnel-ingress` (tags come from the key) |
| `FUNNEL_CONTROL_URL` | headscale URL (control server of the embedded node) |
| `FUNNEL_CONFIG_URL` / `FUNNEL_CONFIG_FILE` | exactly one: node-control base URL (dialled over the tailnet; use its tailnet IP, containers have no tailnet DNS) or a local JSON file |
| `FUNNEL_HOSTNAME` | name on the tailnet, default `funnel-ingress` |
| `FUNNEL_TAG`, `FUNNEL_NS_TAG_PREFIX` | publisher tag and namespace tag prefix, defaults `tag:funnel`, `tag:ns-` |
| `FUNNEL_SYNC_INTERVAL` | config pull period, default `30s` (min `5s`), jittered, with backoff while failing |
| `FUNNEL_PROXY_ADDR`, `FUNNEL_API_PORT` | public listener (default `:8080`) and tailnet API port (default `9100`) |
| `FUNNEL_RECONCILE_INTERVAL`, `FUNNEL_MISSING_CHECKS` | see Lifecycle |
| `FUNNEL_MAX_ROUTES_PER_NODE`, `FUNNEL_MAX_BODY_BYTES` | defaults `20`, `33554432` |
| `FUNNEL_CONNECT_TIMEOUT`, `FUNNEL_IDLE_TIMEOUT`, `FUNNEL_REQUEST_TIMEOUT` | defaults `10s`, `120s`, `300s` |
| `REDIS_URL`, `REDIS_KEY`, `FUNNEL_STATE_KEY`, `STATE_ALLOW_PLAINTEXT`, `STATE_IMPORT_FROM` | state store, see below |
| `FUNNEL_STATE_DIR` | file state only (no `REDIS_URL`), default `/data` |

Startup failures print one line and exit non-zero; error texts never contain the auth key, the Redis URL or the state key.

## State

With `REDIS_URL` set, the tailscale node's identity, the dynamic routes (field `funnel/routes`) and the last good
configuration (field `funnel/config`) live in **one Redis hash** (default `<app>:tsnet`), encrypted at rest with
`FUNNEL_STATE_KEY` (AES-256-GCM, bound to the hash key and field). The machine has no volume. This is a port of
node-control's state store (`scripts/check-statestore-port.sh` shows drift).

Redis rules (shared instance, billed per command): plain `redis://` on the private network, database 0 only, only
`HELLO 2`, `PING`, `HGET`, `HSET`, `HDEL`, `HLEN`, `HEXISTS` (and `HKEYS` for a forced import), the key prefixed `<app>:`,
no TTL, no polling. Redis is read at start; writes happen only when something actually changed (an unchanged
re-registration or configuration version writes nothing). The connection uses the normal OS dialer, not the tailnet.

- **Generate the key** with `openssl rand -base64 32`, keep it in your password manager first, then set it as a Fly
  secret. `REDIS_URL` without a key refuses to start unless `STATE_ALLOW_PLAINTEXT=1`.
- **Redis unreachable at start:** exit non-zero (Fly retries); the node never registers a new identity because Redis was
  briefly down. After start an outage does not stop traffic; a registration that cannot be saved is refused (`503`).
- **Carrying over an identity:** set `STATE_IMPORT_FROM` to an existing tailscale state file for the first start; it is
  imported (crash-safe, verified) only while the hash is empty.
- **Loss of the entry or key:** delete the hash, redeploy with a valid pre-auth key, delete the stale node in headscale;
  nodes re-create their routes at their next re-assertion. A corrupt route blob stops the start and is never overwritten.
- **Rotation:** deploy with the new key only after re-importing, or delete the hash and re-enrol as above.
- Without `REDIS_URL` the node keeps its state in files under `FUNNEL_STATE_DIR` (a volume), for standalone use.

## Configuration source

`GET {FUNNEL_CONFIG_URL}/ingress/v1/config` with `If-None-Match` returns `200` (`ETag` = `version`) or `304`:

```json
{"version":"…","public_domain":"example.com",
 "namespaces":{"pdl":{"ports":[{"lo":8080,"hi":8080}]}},
 "routes":[{"ns":"pdl","service":"docs","target":"docs-host","port":8080,"scheme":"http","insecure_skip_verify":false}],
 "blocked":[{"ns":"pdl","service":"old-admin"}]}
```

`config.example.json` has the same shape for `FUNNEL_CONFIG_FILE`. A new version is validated completely before it is
applied (a namespace range that contains the ingress's own proxy or API port is rejected); a rejected, unreachable or
garbled source leaves the **last good configuration in force**, including after a restart (it is cached in the state
store and applied before the public listener opens). After every cycle the ingress `POST`s `{FUNNEL_CONFIG_URL}/ingress/v1/state`
(applied version, peer-list status, last reconcile, dynamic routes with owner and online state, recent errors).

## DNS and certificates

Per namespace, once:

1. DNS: `*.pdl.<public domain>` CNAME to the Fly app (or A/AAAA to dedicated IPs).
2. `fly certs add "*.pdl.<public domain>" -a <app>`; wildcard validation is DNS-based, so add the `_acme-challenge.pdl`
   CNAME that Fly prints. A wildcard does not cover `pdl.<public domain>` itself.
3. Check Fly's current certificate pricing and limits before adding many namespaces.

`<public domain>` must not be under the headscale `base_domain` (MagicDNS suffix).

## Tags and ACL

Names are configuration. Example policy entries (node-control generates the per-namespace ones):

- owners for `tag:funnel-ingress` (the proxy), `tag:funnel` (publishers) and `tag:ns-pdl` (namespace nodes);
- `tag:funnel` -> `tag:funnel-ingress:9100` (publishers reach only the registration API);
- `tag:funnel-ingress` -> `tag:ns-pdl:<allowed ports>` (the proxy reaches only the namespace's ports);
- `tag:funnel-ingress` -> the node-control tag on its port (pull configuration, report state, nothing else).

A node becomes a publisher only when an operator tags it (pre-auth key with tags, or `headscale nodes tag`).

## Stock clients and Kubernetes

`tailscale funnel` / `tailscale serve --funnel` cannot work against headscale: through tsnet's own APIs the node has no
funnel or HTTPS capability and no cert domains (`ListenFunnel` fails with "Funnel not available; HTTPS must be
enabled", `CertPair` with "your Tailscale account does not support getting TLS certs"). Nodes publish by calling the
registration API (`register-service.sh`, or plain `curl`).

The Kubernetes operator depends on hosted-control-plane features and is not supported. Use `funnel-sidecar`
(`Dockerfile.sidecar`): it joins as its own **ephemeral**, tagged tsnet node (no privileges, no TUN device, no
`tailscaled`), listens on its tailnet address for each declared service, forwards TCP to the pod or an in-cluster target
and registers `(ns, service, port)`. It re-asserts every `FUNNEL_REREGISTER_INTERVAL` (default `5m`), retries with
backoff (also on `403`/`409`), is ready (`/readyz` on `FUNNEL_READY_ADDR`) only while every service is registered, and
deletes its routes on SIGTERM.

| variable | purpose |
|---|---|
| `FUNNEL_NS`, `FUNNEL_SERVICES` | namespace; `name=port` (forward to `127.0.0.1:port`) or `name=port@host:port2`, comma separated |
| `FUNNEL_API_URL` | the ingress node's API, for example `http://funnel-ingress:9100` |
| `FUNNEL_CONTROL_URL`, `TS_AUTHKEY`, `FUNNEL_HOSTNAME` | control server, key, node name (default `$HOSTNAME`) |
| `FUNNEL_STATE_DIR`, `FUNNEL_READY_ADDR` | scratch dir (`/tmp/funnel-sidecar`), readiness listener (`:8081`) |

- Use **one ephemeral pre-auth key per namespace** carrying `tag:funnel` and `tag:ns-<ns>`: a leaked key can publish only
  inside that namespace.
- One sidecar per `(ns, service)`. For replicated apps run a single-replica forwarder in front of the Kubernetes Service
  (`examples/k8s/forwarder-deployment.yaml`), not one sidecar per replica (a second registrant gets `409`).
- A dead pod's route disappears when headscale expires its ephemeral node (`node.ephemeral.inactivity_timeout` in the
  headscale config; default 30 minutes, minimum 65 s, 2 to 5 minutes is a good value here). Services behind a subnet
  router are not supported.

## Security notes

The registration API is on the tailnet only (the Fly service config exposes just the proxy port); the ACL, not code,
is the outer wall for what the proxy can reach. The public listener serves the proxy and `/healthz` only (for hosts that
are not service hosts); there is no pprof or metrics endpoint on it.

## Build and test

```sh
go vet ./... && go test -race ./...
docker build .                                   # the ingress
docker build -f Dockerfile.sidecar .             # the sidecar
go test -tags integration ./internal/tsnode/     # needs FUNNEL_TEST_CONTROL_URL and FUNNEL_TEST_AUTHKEY
```

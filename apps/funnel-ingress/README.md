# funnel-ingress

This app publishes services of tailnet nodes on the public internet under stable names, the way Tailscale Funnel does.
It is for a self-hosted headscale network. headscale does not implement Funnel. It has no `funnel` node attribute, no
control-issued certificates and no relay fleet. For this reason, `tailscale funnel` on a client cannot work here. This app is
the equivalent.

It is a small public reverse proxy on its own Fly app. It joins the tailnet as one more node.

```
Internet -> Fly edge (TLS, wildcard certs) -> funnel-ingress machine (Go, embedded tailscale node)
                                               |- public listener  :8080  reverse proxy, routed by Host
                                               |- tailnet listener :9100  registration API (not on the internet)
                                               |- reconcile loop          peer list vs. registered routes
                                               '- sync loop              pulls config from node-control, reports state
                                               v dials over the tailnet
                                     target node (tag:funnel + tag:ns-<ns>) :port
```

A service `admin` in namespace `pdl` is served at `https://admin.pdl.<public domain>/`. Each namespace has one wildcard record and one
wildcard certificate. When you add a service afterwards, you change no DNS and no certificate.

**Single admin surface:** the ingress has no admin API, page, tokens or users. You manage namespaces, their allowed ports,
manual routes and blocked names in node-control. The ingress pulls them. node-control never calls
the ingress. Without node-control, a local JSON file of the same shape works (`FUNNEL_CONFIG_FILE`).

## Routing

Host `<service>.<namespace>.<public domain>` maps to a route. There are exactly two labels before the domain.
The proxy normalises case, a port and one trailing dot. Any other host gets a bare `404`.

The proxy forwards the original `Host`. It rebuilds `X-Forwarded-For/Host/Proto` from scratch. It drops client-supplied forwarding headers.

The proxy reads the client address from one header. By default, this is the `Fly-Client-IP` header that the edge of Fly sets.
Behind a different load balancer, set `FUNNEL_CLIENT_IP_HEADER` and `FUNNEL_TRUSTED_PROXIES` (see the table).

The proxy reads the header and `X-Forwarded-Proto` only from a trusted peer. For `X-Forwarded-For`, the client is the
right-most address that is not a trusted proxy. If the header is missing or not valid, the proxy uses the address of the peer.

**WARNING:** if `FUNNEL_TRUSTED_PROXIES` is empty, the proxy reads the header from all peers. Use an empty value only if
the load balancer is the only route to the listener. Otherwise, a client can send a false address.

The proxy strips hop-by-hop headers. WebSocket upgrades work.
Connect, idle and response-header timeouts and a request-body limit apply.

A known route whose backend is unreachable
answers `503` (`502` for a broken backend). The answer never contains internal names or addresses.

Backends are `http` by default, because the tailnet is already encrypted. A static route can use `"scheme": "https"`. This
verifies the certificate against the target address. A static route can also use `"insecure_skip_verify": true`.

Two sources feed one route table, with fixed precedence:

- **static routes** from the configuration source. They are read-only to the API and they always win. A registration for the
  same name gets `409`.
- **dynamic routes** that nodes register. The ingress saves them in the state store.
- **blocked names** from the configuration. The ingress refuses registration (`403`) and removes an existing dynamic route.

## Registration API (tailnet only, default port 9100)

The identity of the caller comes from the control plane (`WhoIs` on the connecting tailnet address), never from the
request. A node can publish only when **all** of these conditions are true:

1. The node carries the funnel tag (`FUNNEL_TAG`, default `tag:funnel`).
2. The node carries a namespace tag (`FUNNEL_NS_TAG_PREFIX`, default `tag:ns-`, so `tag:ns-pdl` is namespace `pdl`).
   The namespaces that it can publish are exactly those tags. With several tags, the request must name one.
3. The port is inside the allowed ranges of the namespace. An empty list rejects everything.

The backend of a route is always the **tailnet address of the caller**. A body cannot name another address.

| call | meaning |
|---|---|
| `PUT /v1/routes/{service}` body `{"ns":"pdl","port":8080,"scheme":"http"}` | create or update (idempotent for the same node) |
| `DELETE /v1/routes/{service}?ns=pdl` | withdraw (owner only) |
| `GET /v1/routes` | the caller's own routes |

Answers carry no body. The statuses are:

- `204` or `200`
- `401` (unknown caller)
- `403` (tags, namespace, port, blocked name)
- `409` (owned by another node, or a static route)
- `422` (bad input)
- `429` (per-node route limit)
- `503` (could not be saved: the ingress refused the request and nothing changed)

You cannot publish ports of the ingress itself (proxy, API). The proxy node cannot
register itself.

Helper for a node: `register-service.sh [--scheme https] SERVICE PORT [NAMESPACE]` and `register-service.sh --delete
SERVICE [NAMESPACE]` (needs `FUNNEL_API_HOST`; idempotent, run it at boot or from a timer).

## Lifecycle

A loop (`FUNNEL_RECONCILE_INTERVAL`, default 60 s) compares dynamic routes with the peer list of the proxy node:

- Owner **present but offline**: the route stays (requests get `503`). There is no heartbeat expiry.
- Owner **absent for `FUNNEL_MISSING_CHECKS` consecutive checks** (default 3), or **no longer carrying the funnel tag or
  that namespace tag**: the ingress removes all its routes. It logs each route (`node, ns, service`).
- Routes are bound to the **stable node ID**. A deleted and re-enrolled node with the same name inherits nothing.
- After a start, the ingress cleans up nothing until it loads the first complete peer list.

The headscale ACL filters the peer list. If an ACL change hides a node from the proxy, the ingress removes its routes.
This is intended, because the proxy could not reach the node anyway.

## Environment

Put secrets in Fly secrets. Put plain settings in `fly.toml`. Namespaces, ports, the public domain and routes are **not**
process settings. They arrive through the configuration source.

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
| `FUNNEL_CLIENT_IP_HEADER` | header with the client address, default `Fly-Client-IP`. `X-Forwarded-For` is read as a chain. `none` uses the peer address only |
| `FUNNEL_TRUSTED_PROXIES` | comma-separated CIDRs or addresses of the load balancer. Only these peers can set the client header and `X-Forwarded-Proto`. Empty: all peers (see the warning above) |
| `REDIS_URL`, `REDIS_KEY`, `FUNNEL_STATE_KEY`, `STATE_ALLOW_PLAINTEXT`, `STATE_IMPORT_FROM` | state store, see below |
| `FUNNEL_STATE_DIR` | file state only (no `REDIS_URL`), default `/var/lib/tailscale` (as in node-control; the image creates it for the app user) |

A startup failure prints one line and exits non-zero. Error texts never contain the auth key, the Redis URL or the state key.

## State

When `REDIS_URL` is set, **one Redis hash** (default `<app>:tsnet`) holds three items. They are the identity of the tailscale node,
the dynamic routes (field `funnel/routes`) and the last good configuration (field `funnel/config`). The ingress encrypts the hash at rest with
`FUNNEL_STATE_KEY` (AES-256-GCM, bound to the hash key and field). The machine has no volume. This is a port of
the state store of node-control (`scripts/check-statestore-port.sh` shows drift).

Redis rules (shared instance, billed for each command):

- Use plain `redis://` on the private network, database 0 only.
- Use only these commands: `HELLO 2`, `PING`, `HGET`, `HSET`, `HDEL`, `HLEN`, `HEXISTS` (and `HKEYS` for a forced import).
- Prefix the key with `<app>:`. Use no TTL and no polling.

The ingress reads Redis at start. It writes only when something changed. An unchanged
re-registration or configuration version writes nothing. The connection uses the normal OS dialer, not the tailnet.

- **Generate the key** with `openssl rand -base64 32`. Keep it in your password manager first. Then set it as a Fly
  secret. `REDIS_URL` without a key refuses to start, unless `STATE_ALLOW_PLAINTEXT=1`.
- **Redis unreachable at start:** the ingress exits non-zero (Fly retries). The node never registers a new identity because Redis was
  briefly down. After the start, an outage does not stop traffic. The ingress refuses a registration that it cannot save (`503`).
- **Carrying over an identity:** for the first start, set `STATE_IMPORT_FROM` to an existing tailscale state file. The ingress
  imports it (crash-safe, verified) only while the hash is empty.
- **Loss of the entry or key:** delete the hash. Redeploy with a valid pre-auth key. Delete the stale node in headscale.
  Nodes re-create their routes at their next re-assertion. A corrupt route blob stops the start. The ingress never overwrites it.
- **Rotation:** deploy with the new key only after you re-import. Or delete the hash and re-enrol as above.
- Without `REDIS_URL`, the node keeps its state in files under `FUNNEL_STATE_DIR` (a volume), for standalone use.

## Configuration source

`GET {FUNNEL_CONFIG_URL}/ingress/v1/config` with `If-None-Match` returns `200` (`ETag` = `version`) or `304`:

```json
{"version":"…","public_domain":"example.com",
 "namespaces":{"pdl":{"ports":[{"lo":8080,"hi":8080}]}},
 "routes":[{"ns":"pdl","service":"docs","target":"docs-host","port":8080,"scheme":"http","insecure_skip_verify":false}],
 "blocked":[{"ns":"pdl","service":"old-admin"}]}
```

`config.example.json` has the same shape for `FUNNEL_CONFIG_FILE`. The ingress validates a new version completely before it applies it.
It rejects a namespace range that contains the own proxy or API port of the ingress.

If the source is rejected, unreachable or garbled, the **last good configuration stays in force**. This is also true after a restart.
The ingress caches the configuration in the state store and applies it before the public listener opens.

After every cycle, the ingress sends a `POST` to `{FUNNEL_CONFIG_URL}/ingress/v1/state`
(applied version, peer-list status, last reconcile, dynamic routes with owner and online state, recent errors).

## DNS and certificates

Per namespace, once:

1. DNS: add `*.pdl.<public domain>` CNAME to the Fly app (or A/AAAA to dedicated IPs).
2. Run `fly certs add "*.pdl.<public domain>" -a <app>`. Wildcard validation is DNS-based, so add the `_acme-challenge.pdl`
   CNAME that Fly prints. A wildcard does not cover `pdl.<public domain>` itself.
3. Check the current certificate pricing and limits of Fly before you add many namespaces.

`<public domain>` must not be under the headscale `base_domain` (MagicDNS suffix).

## Tags and ACL

Names are configuration. These are example policy entries (node-control generates the entries for each namespace):

- Owners for `tag:funnel-ingress` (the proxy), `tag:funnel` (publishers) and `tag:ns-pdl` (namespace nodes).
- `tag:funnel` -> `tag:funnel-ingress:9100`: publishers reach only the registration API.
- `tag:funnel-ingress` -> `tag:ns-pdl:<allowed ports>`: the proxy reaches only the ports of the namespace.
- `tag:funnel-ingress` -> the node-control tag on its port: pull configuration, report state, nothing else.

A node becomes a publisher only when an operator tags it (pre-auth key with tags, or `headscale nodes tag`).

## Stock clients and Kubernetes

`tailscale funnel` and `tailscale serve --funnel` cannot work against headscale. Through the own APIs of tsnet, the node has no
funnel or HTTPS capability and no cert domains. `ListenFunnel` fails with "Funnel not available; HTTPS must be
enabled". `CertPair` fails with "your Tailscale account does not support getting TLS certs". Nodes publish by calling the
registration API (`register-service.sh`, or plain `curl`).

The Kubernetes operator depends on hosted-control-plane features. It is not supported. Use `funnel-sidecar`
(`Dockerfile.sidecar`). It joins as its own **ephemeral**, tagged tsnet node (no privileges, no TUN device, no
`tailscaled`). It listens on its tailnet address for each declared service. It forwards TCP to the pod or an in-cluster target
and registers `(ns, service, port)`.

CI publishes the image as `ghcr.io/valentinalexeev/headlamp/funnel-sidecar` for `linux/amd64` and `linux/arm64`.
[VERSIONING.md](../../VERSIONING.md#images) lists the tags.

It re-asserts every `FUNNEL_REREGISTER_INTERVAL` (default `5m`). It retries with
backoff (also on `403` and `409`). It is ready (`/readyz` on `FUNNEL_READY_ADDR`) only while every service is registered. It
deletes its routes on SIGTERM.

`/readyz` gives `200` when the last registration round was successful, and `503` when it was not. The JSON body
contains `ready`, `last_registered` (the time of the last successful round, RFC 3339) and `age_seconds` (the seconds since
that round). Before the first successful round, the body does not contain `last_registered` and `age_seconds`.

If the registration API gives no answer for `FUNNEL_UNREACHABLE_EXIT` (default `5m`), the sidecar deletes its routes
and stops with an error. A `403` or `409` is an answer, so it does not start this timer. The usual cause is that headscale
removed the ephemeral node. The tsnet node does not log in again, so only a new process recovers. Set `0` to disable the
exit.

Kubernetes restarts the container and the new process logs in again with `TS_AUTHKEY`. Thus the key must be reusable (an
ephemeral key is reusable). The sidecar also logs each change of the tailnet node state and its health
warnings (`tailnet: node status`).

| variable | purpose |
|---|---|
| `FUNNEL_NS`, `FUNNEL_SERVICES` | namespace; `name=port` (forward to `127.0.0.1:port`) or `name=port@host:port2`, comma separated |
| `FUNNEL_API_URL` | the ingress node's API, for example `http://funnel-ingress:9100` |
| `FUNNEL_CONTROL_URL`, `TS_AUTHKEY`, `FUNNEL_HOSTNAME` | control server, key, node name (default `$HOSTNAME`) |
| `FUNNEL_STATE_DIR`, `FUNNEL_READY_ADDR` | scratch dir (`/tmp/funnel-sidecar`), readiness listener (`:8081`) |
| `FUNNEL_REREGISTER_INTERVAL` | interval between registrations (`5m`, minimum `10s`) |
| `FUNNEL_UNREACHABLE_EXIT` | stop after the API gives no answer for this time (`5m`, minimum `1m`, `0` disables) |

- Use **one ephemeral pre-auth key for each namespace**. The key carries `tag:funnel` and `tag:ns-<ns>`. A leaked key can publish only
  inside that namespace.
- One sidecar can publish several services of one namespace. Put each service in `FUNNEL_SERVICES` with its own name
  and its own tailnet port, for example `web=8080@web.demo.svc:80,hook=8081@hook.demo.svc:80`. The targets can be in
  different Kubernetes namespaces. All the routes go into the namespace of the sidecar (`FUNNEL_NS`), and together they
  count toward `FUNNEL_MAX_ROUTES_PER_NODE`. `/readyz` is ready only when all of them are registered. SIGTERM deletes all of them.
- Each service name has one registrant. For replicated apps, run a single-replica forwarder in front of the Kubernetes
  Service (`examples/k8s/forwarder-deployment.yaml`). Do not run one sidecar for each replica, because a second
  registrant gets `409`.
- The route of a dead pod disappears when headscale expires its ephemeral node (`node.ephemeral.inactivity_timeout` in the
  headscale config). The default is 30 minutes and the minimum is 65 s. A value of 2 to 5 minutes is good here.
  Services behind a subnet router are not supported.

## Security notes

The registration API is on the tailnet only. The Fly service config exposes just the proxy port. The ACL, not code,
is the outer wall for what the proxy can reach. The public listener serves the proxy and `/healthz` only (for hosts that
are not service hosts). There is no pprof or metrics endpoint on it.

## Build and test

```sh
go vet ./... && go test -race ./...
docker build .                                   # the ingress
docker build -f Dockerfile.sidecar .             # the sidecar
go test -tags integration ./internal/tsnode/     # needs FUNNEL_TEST_CONTROL_URL and FUNNEL_TEST_AUTHKEY
```

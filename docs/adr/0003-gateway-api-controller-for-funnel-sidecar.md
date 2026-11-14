# ADR 0003: A Gateway API controller for funnel-sidecar

Status: accepted. The owner accepted the decisions on #338 ("as proposed"). Milestone 0.19.0.

## Context

An operator publishes each in-cluster Service by hand. A serve proxy needs one entry in `serve.services[]` of the
`tailnet-connector` chart. `funnel-sidecar` needs its targets in `FUNNEL_SERVICES`. An error shows only in the
logs and in `/readyz`. A target in a different namespace needs no approval on the Kubernetes side.

Issue #338 proposed a controller that reads Gateway API resources. The owner accepted these decisions:

1. Scope: phases 1 (documentation) and 2 (the controller). Phase 3 (automatic keys) is not done.
2. Package: a new chart `charts/gateway-controller` and a new image `gateway-controller`.
3. Keys: the operator makes each key in node-control. The `Gateway` names the key Secret in
   `spec.infrastructure.parametersRef`.
4. Routes: `HTTPRoute` and `TCPRoute`. `TCPRoute` only when its CRD is installed.
5. Default: the chart does nothing until an operator makes a `Gateway` of its class.

## Decision

### Model

| Gateway API | headlamp |
|---|---|
| `GatewayClass` `headlamp-serve` | `funnel-sidecar` in serve mode: tailnet-only names with TLS |
| `GatewayClass` `headlamp-funnel` | `funnel-sidecar` registers public names with `funnel-ingress` |
| `Gateway` | One Deployment `<gateway>-funnel` of `funnel-sidecar`, one replica, strategy `Recreate`. One tailnet node |
| `parametersRef` (core `Secret`) | `TS_AUTHKEY` through `secretKeyRef`, key `authkey` |
| Listener | One entry of `FUNNEL_SERVICES`: the first label of the hostname (or the listener name for `TCP`), and the listener port as the tailnet port |
| Route `backendRef` | `<ClusterIP>:<port>` of the Service, the target of the entry |
| Serve listener `tls.certificateRefs` | `FUNNEL_SERVE_CERT_FILE` and `FUNNEL_SERVE_KEY_FILE` from a mounted Secret. Without it: `FUNNEL_SERVE_URL` (node-control) |

The target is the ClusterIP, not the pod addresses. kube-proxy balances the connections, so a replicated app needs
no forwarder Deployment.

### What the sidecar cannot do

The sidecar forwards a whole name to one backend. These routes get `Accepted=False` with reason `UnsupportedValue`
and a message:

- a route with a path, header, query or method match,
- a route with a filter,
- a route with more than one rule or `backendRef`.

`PathPrefix /` is the same as no match, and is accepted. For L7 rules, the operator puts a Gateway API
implementation behind a wildcard serve service (phase 1).

One listener takes one route. The oldest route (by creation time, then by kind, namespace and name) gets the
listener. The others get `Accepted=False` with reason `NotAllowedByListeners`. Thus the choice is stable when the
controller restarts.

### Security

- A cross-namespace `backendRef` needs a `ReferenceGrant` in the target namespace, from the route kind and
  namespace, to the Service (or to all Services). Else the route gets `ResolvedRefs=False` with reason
  `RefNotPermitted`, and the target is not published.
- The controller does not read Secrets. The pod reads the key itself. The controller cannot copy a key.
- The ClusterRole gives read access to the Gateway API objects, Services and Pods, write access to their status,
  and access to Deployments. The cache holds only the pods and Deployments with the label
  `app.kubernetes.io/managed-by: headlamp-gateway-controller`.
- The sidecar pod has no service account token, runs as a non-root user, has a read-only root file system and
  drops all capabilities.

### Status

- `Accepted` on the `Gateway`: `parametersRef` names a Secret, and at least one listener is valid.
- `Programmed` on the `Gateway`: the controller reads `http://<pod IP>:8081/readyz` of the sidecar pod. When the
  body has `ready: true` and at least one address, `Programmed` is `True` and `status.addresses` has the tailnet
  addresses (type `IPAddress`). `funnel-sidecar` 0.19.0 adds `addresses` to the `/readyz` body for this.
- A route gets one `RouteParentStatus` for each `parentRef` that names a `Gateway` of the controller, with
  `Accepted` and `ResolvedRefs`. `RouteParentStatus` has no address field, so the `Accepted` message gives the
  tailnet address.
- The controller writes a status only when it changes.

### Implementation

- Go, `controller-runtime` and the typed Gateway API module. No CRD of our own.
- The translation (`internal/translate`) has no client. Unit tests check it, and the reconciler is tested with the
  `controller-runtime` fake client.
- A change to a route, Service, `ReferenceGrant`, `GatewayClass` or sidecar pod reconciles all Gateways. A cluster
  has few Gateways, and a full pass keeps the choice of "one route for each listener" correct.
- One controller replica, no leader election. A second replica would only write the same status.
- With no route, the Deployment has 0 replicas: `funnel-sidecar` refuses an empty `FUNNEL_SERVICES`.

## Consequences

- A new app needs a `Gateway`, a key Secret and an `HTTPRoute`, and no `helm upgrade`.
- Errors show in the status of the Kubernetes objects.
- The `tailnet-connector` chart does not change.
- A person who can make a `Gateway` of a headlamp class can use any key Secret of its namespace. The chart README
  has a warning.
- Phase 3 (automatic keys) needs a new decision about the trust model. It is not in this change.

## Checks

- `cd apps/gateway-controller && go test ./...`
- `make test-gateway-controller` (chart static checks and RBAC)
- `make test-gateway-controller-e2e` (kind and headscale)

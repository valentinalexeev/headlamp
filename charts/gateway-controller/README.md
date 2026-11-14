# gateway-controller chart

This chart installs a Kubernetes controller. The controller reads Gateway API resources and publishes
in-cluster Services on the tailnet (#338). For each `Gateway`, it makes one Deployment of `funnel-sidecar` and
writes the sidecar configuration (`FUNNEL_SERVICES`). Thus you do not edit `serve.services[]` or
`FUNNEL_SERVICES` by hand.

The design is in [ADR 0003](../../docs/adr/0003-gateway-api-controller-for-funnel-sidecar.md).

The chart is off by default: it does nothing until you make a `Gateway` of one of its classes. A site that does
not install the chart does not change.

## Requirements

- The Gateway API CRDs, standard channel, v1.2 or newer. Install them before the chart.
- `TCPRoute` is optional. The controller uses it only when its CRD is installed when the controller starts. If you
  install the CRD later, restart the controller.
- For each `Gateway`: a node-control service and its key. You make them in node-control, as for a serve proxy or a
  `funnel-sidecar`. The controller does not make keys.

## Classes

The chart makes two GatewayClasses. Their `controllerName` is
`github.com/valentinalexeev/headlamp/gateway-controller`.

| Class | Mode | Listener protocols | Route kinds |
|---|---|---|---|
| `headlamp-serve` | Tailnet-only names with TLS (`funnel-sidecar` serve mode) | `HTTPS`, `TCP` | `HTTPRoute`, `TCPRoute` |
| `headlamp-funnel` | Public names through `funnel-ingress` | `HTTP`, `TCP` | `HTTPRoute`, `TCPRoute` |

## Mapping

| Gateway API | headlamp |
|---|---|
| `Gateway` | One tailnet node: one key, one node-control service, one `funnel-sidecar` Deployment `<gateway>-funnel` |
| `spec.infrastructure.parametersRef` | The Secret with the key (key `authkey`), in the namespace of the `Gateway` |
| `listeners[].hostname` | The service name: the first label (`grafana.ts.example.com` gives `grafana`) |
| `listeners[].name` | The service name of a `TCP` listener (it has no hostname) |
| `listeners[].port` | The tailnet port |
| `backendRefs` of a route | The target: `<ClusterIP>:<port>` of the Service |
| `tls.certificateRefs` (serve class only) | The certificate Secret of the sidecar (`FUNNEL_SERVE_CERT_FILE`). Without it, the sidecar gets the certificate from node-control |
| `Gateway` status `addresses` | The tailnet addresses of the sidecar node (from its `/readyz`) |

## Limits

The sidecar sends a whole name to one backend. It does not route by path or header. The controller sets
`Accepted=False` with reason `UnsupportedValue` on a route that has one of these:

- a path match other than `PathPrefix /`,
- a header, query parameter or method match,
- a filter (on the rule or on a `backendRef`),
- more than one rule, or more than one `backendRef`.

For these rules, put a Gateway API implementation behind a wildcard serve service. See "A Gateway API
implementation behind one serve proxy" in the [tailnet-connector chart README](../tailnet-connector/README.md).

Other limits:

- One listener takes one route. The oldest route gets the listener. A newer route gets `Accepted=False` with
  reason `NotAllowedByListeners`.
- A wildcard hostname (`*.apps.example.com`) is not supported on a listener.
- Each listener must have a different service name and a different port.
- A `backendRef` must name a Service with a ClusterIP. A headless Service or an `ExternalName` Service gets
  `ResolvedRefs=False`.
- `allowedRoutes.namespaces.from` must be `Same` (the default) or `All`.

## Cross-namespace references

A route can name a Service in a different namespace only when a `ReferenceGrant` in that namespace permits it.
Without a grant, the route gets `ResolvedRefs=False` with reason `RefNotPermitted`, and the controller does not
publish it.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: ReferenceGrant
metadata: { name: from-apps, namespace: monitoring }
spec:
  from: [{ group: gateway.networking.k8s.io, kind: HTTPRoute, namespace: apps }]
  to: [{ group: "", kind: Service, name: grafana }]
```

## Status

| Object | Condition | Meaning |
|---|---|---|
| `GatewayClass` | `Accepted` | The class is one of the two classes of the chart |
| `Gateway` | `Accepted` | The `parametersRef` is a Secret, and at least one listener is valid |
| `Gateway` | `Programmed` | The sidecar pod is ready, and it has a tailnet address. `status.addresses` has the addresses |
| Listener | `Accepted`, `Conflicted`, `ResolvedRefs`, `Programmed` | The result for each listener |
| Route | `Accepted` | The route has a listener. The message gives the tailnet address |
| Route | `ResolvedRefs` | The Service exists, has the port, and a `ReferenceGrant` permits it if necessary |

`Programmed` is `False` with reason `Pending` while the sidecar starts, and while no route is attached. With no
route, the sidecar Deployment has 0 replicas.

## Install

1. Install the Gateway API CRDs.
2. Install the chart:

   ```sh
   helm install gateway-controller charts/gateway-controller -n gateway-controller --create-namespace \
     --set controlURL=https://headscale.example.com \
     --set serveURL=http://100.64.0.1
   ```

3. In node-control, make the service and its key. For the serve class, use the key of a serve service. For the
   funnel class, use a key with the tag `tag:ns-<namespace>`.
4. Put the key in a Secret, in the namespace of the `Gateway`, with the key `authkey`.
5. Make the `Gateway` and the `HTTPRoute`. See [examples/gateway-serve.yaml](examples/gateway-serve.yaml).
6. Check the status:

   ```sh
   kubectl -n monitoring get gateway grafana -o jsonpath='{.status.addresses[*].value}'
   ```

## Values

| Value | Default | Description |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest` | `ghcr.io/valentinalexeev/headlamp/gateway-controller`, chart version, empty | The controller image. Pin a digest in production |
| `sidecarImage.*` | `ghcr.io/valentinalexeev/headlamp/funnel-sidecar`, chart version, empty | The image of each sidecar |
| `gatewayClasses.create` | `true` | Make the two GatewayClasses |
| `gatewayClasses.serve`, `gatewayClasses.funnel` | `headlamp-serve`, `headlamp-funnel` | The class names |
| `controlURL` | empty | headscale (`FUNNEL_CONTROL_URL`). Empty: the tailscale default |
| `serveURL` | `http://100.64.0.1` | Serve class: node-control on the tailnet (`FUNNEL_SERVE_URL`) |
| `funnelAPIURL` | empty | Funnel class: the funnel-ingress registration API (`FUNNEL_API_URL`) |
| `caBundle.configMap` | empty | A ConfigMap with `ca.crt`, in the namespace of each `Gateway`, for a private CA |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | small requests | For the controller pod |

## Permissions

The ClusterRole of the controller gives:

- read access to GatewayClasses, Gateways, HTTPRoutes, TCPRoutes, ReferenceGrants, Services and Pods,
- write access to the status of GatewayClasses, Gateways, HTTPRoutes and TCPRoutes,
- full access to Deployments (the sidecars).

The controller cannot read Secrets. The sidecar pod reads its key Secret itself, through `secretKeyRef`. The
controller caches only the pods and Deployments that it made.

**WARNING:** A person who can make a `Gateway` of a headlamp class in a namespace can run a sidecar with any key
Secret of that namespace. Give the permission to make `Gateway` objects only to persons who can read the Secrets of
that namespace.

## Tests

- `cd apps/gateway-controller && go test ./...`: the translation, the conditions, `ReferenceGrant` and the
  unsupported matches, with a fake client.
- `make test-gateway-controller`: `helm lint`, `helm template`, kubeconform and the RBAC checks.
- `make test-gateway-controller-e2e`: the chart in kind against the pinned headscale. A `Gateway` and an
  `HTTPRoute` publish a ClusterIP Service on the tailnet, the `Gateway` gets `Programmed=True` and the address, a
  tailnet peer reaches the Service, and a route with a path match gets `Accepted=False`.

# tailnet-connector

A Helm chart for a Kubernetes cluster that joins a headscale tailnet. It has two parts:

- **The connector.** It is a subnet router for the cluster. Tailnet devices can then reach the Service range and the Pod range of the cluster. It is the headlamp equivalent of a "Connector" of the Tailscale Kubernetes operator. That operator does not work with headscale.
- **Serve proxies (optional, off by default).** Each proxy gives one in-cluster Service a tailnet-only name with HTTPS. It is `funnel-sidecar` in serve mode (#117). See "Serve module" in the node-control README.

## How the connector works

- One Deployment with one replica runs tailscale in userspace mode. It needs no capabilities, no `/dev/net/tun` and no root.
- It joins the tailnet with a pre-auth key from a Secret. The key is reusable, ephemeral and tagged. The key gives the node its tag. You create that Secret. The chart never creates it.
- It advertises the CIDRs in `connector.routes` (`TS_ROUTES`).
- headscale approves the routes through the policy (`autoApprovers.routes`). Nobody approves them by hand.
- It keeps its tailscale state in a Kubernetes Secret (`TS_KUBE_SECRET`). The chart creates that Secret empty. A restart keeps the same node and the same tailnet address.
- A Role lets the pod get, update and patch that one Secret. It gives no other access.

The image is `ghcr.io/valentinalexeev/headlamp/tailnet-connector`. It contains `containerboot`, `tailscaled` and `tailscale` at the version in `TAILSCALE_VERSION`. The chart `appVersion` is that version, and `make check-tailscale-pins` checks it. When `image.tag` is empty, the chart uses its own version as the tag.

## Values

| Value | Default | Meaning |
|---|---|---|
| `controlURL` | `https://headscale.example.com` | The headscale URL |
| `connector.enabled` | `true` | Install the connector |
| `connector.hostname` | the release name | The node name in the tailnet |
| `connector.routes` | `[10.96.0.0/12]` | The CIDRs to advertise. Use the Service range and, if necessary, the Pod range |
| `connector.authKeySecret.name`, `.key` | `tailnet-connector-authkey`, `authkey` | The Secret with the pre-auth key |
| `connector.stateSecret` | `<release>-state` | The Secret for the tailscale state |
| `connector.extraArgs` | `[]` | More flags for `tailscale up` |
| `image.repository`, `.tag`, `.digest` | the headlamp image, the chart version, empty | Set a digest in production |
| `caBundle.configMap`, `.key` | empty, `ca.crt` | A ConfigMap with the CA of headscale, if headscale uses a private CA |
| `serve.enabled` | `false` | Install the serve proxies |
| `serve.url` | `http://100.64.0.1` | node-control on the tailnet (`FUNNEL_SERVE_URL`) |
| `serve.services[]` | empty | One entry for each node-control service: `name`, `services` (`FUNNEL_SERVICES`), `replicas`, `authKeySecret`, `dnsNames` (cert-manager only) |
| `serve.certificate.source` | `node-control` | Where the proxies get their certificate: `node-control` or `cert-manager`. See "Serve certificates" |
| `serve.certificate.zone` | empty | cert-manager only: the serve zone. The default name of a proxy is `<name>.<zone>` |
| `serve.certificate.issuerRef` | `name: ""`, `kind: ClusterIssuer`, `group: cert-manager.io` | cert-manager only: your Issuer or ClusterIssuer |

`values.schema.json` checks the values. A route must be a CIDR. The URL must start with `http://` or `https://`.

## The policy

Give the connector tag an owner, and let it approve only the ranges of this cluster. Add one prefix for each range in `connector.routes`:

```json
{
  "tagOwners": {
    "tag:k8s-connector": ["_svcs@"]
  },
  "autoApprovers": {
    "routes": {
      "10.96.0.0/12": ["tag:k8s-connector"],
      "10.244.0.0/16": ["tag:k8s-connector"]
    }
  }
}
```

headscale v0.29 approves a route when a prefix in `autoApprovers.routes` contains it. A range that is not in the policy stays unapproved. Thus a node with a different tag cannot take the ranges of the cluster.

**WARNING:** Do not give the connector tag a large prefix, for example `0.0.0.0/1`. With a large prefix, a stolen connector key can take routes for other networks.

The ACL must also let the users reach the ranges, for example `{"action": "accept", "src": ["group:k8s-admins"], "dst": ["10.96.0.0/12:*"]}`. With more than one cluster, use a different tag for each cluster, and ranges that do not overlap.

## Install the connector

1. Find the Service range and the Pod range of the cluster.
2. Add the policy entries above to the headscale policy.
3. Make a pre-auth key for the connector:

   ```sh
   headscale preauthkeys create -u <owner of the tag> --reusable --ephemeral -e 2160h --tags tag:k8s-connector
   ```

4. Put the key in a Secret in the namespace of the release:

   ```sh
   kubectl -n tailnet create secret generic tailnet-connector-authkey --from-literal=authkey=<key>
   ```

5. Write a values file in your private repo. Use `examples/values-site.yaml` as a start.
6. Install the chart:

   ```sh
   helm upgrade --install tc oci://ghcr.io/valentinalexeev/headlamp/charts/tailnet-connector --version X.Y.Z \
     -n tailnet -f values-site.yaml
   ```

   Use the version of a headlamp release. To install the chart from a checkout of the repo, use
   `charts/tailnet-connector` as the chart.

7. Make sure that the node is online with the routes approved:

   ```sh
   headscale nodes list-routes
   ```

8. On a tailnet device, turn on `--accept-routes`, and open a Service by its ClusterIP.

**NOTE:** The connector does not give DNS for `cluster.local` names. Use the ClusterIP, or use the serve proxies for names.

## Rotate the key

The connector uses the key only when it logs in. It logs in at the first start. It also logs in after headscale removes the ephemeral node. headscale removes an ephemeral node when it is offline for longer than `node.ephemeral.inactivity_timeout`. Thus the key must stay valid while the connector runs.

**CAUTION:** Do not expire the old key before the Secret has the new key. If the connector logs in again, it uses the key in the Secret.

1. Make a new key with the same tags (step 3 of the install).
2. Replace the key in the Secret:

   ```sh
   kubectl -n tailnet create secret generic tailnet-connector-authkey --from-literal=authkey=<new key> \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

3. Restart the connector:

   ```sh
   kubectl -n tailnet rollout restart deploy/tc
   ```

4. Make sure that the node is online, with the same name and address.
5. Expire the old key:

   ```sh
   headscale preauthkeys expire -u <owner of the tag> <old key>
   ```

To get a new node identity too, delete the data in the state Secret before step 3. Then delete the old node in headscale.

## Serve proxies

Each entry in `serve.services` is one Deployment of `funnel-sidecar` in serve mode. The `name` is the name of the service in node-control, without `*.` for a wildcard service. The chart uses it in the names of the Deployment and of the Secret. The default Secret name is `<release>-serve-<name>`, with the key `authkey`.

1. In node-control, add the service, for example `grafana`, or the wildcard `*.apps` for an ingress controller.
2. Copy the key that node-control shows. The key is reusable and ephemeral, with the tags `tag:serve` and `tag:ns-<namespace>`.
3. Put the key in the Secret of the service:

   ```sh
   kubectl -n tailnet create secret generic tc-serve-grafana --from-literal=authkey=<key>
   ```

4. Add the service to the values, and set `serve.enabled: true`.
5. Run `helm upgrade` again.

All replicas of a service use the same key. The name of the service answers with all replicas that are online. To rotate the key, add a new key in node-control and replace it in the Secret. Then restart the Deployment, and retire the old key in node-control.

### A Gateway API implementation behind one serve proxy

One serve proxy can publish many in-cluster services. Put a wildcard serve service in front of the Service of a
Gateway API implementation (or of an ingress controller). The implementation then routes by host name and path. The
proxy forwards TCP only.

1. Install a Gateway API implementation, for example Envoy Gateway, Istio, Cilium or NGINX Gateway Fabric. Make a
   `Gateway` with a listener `protocol: HTTP` on port 80 and `hostname: "*.apps.<serve zone>"`. Note the Service that
   the implementation makes for this `Gateway`.
2. In node-control, add the wildcard serve service `*.apps`. Copy its key into a Secret, as in steps 2 and 3 above.
3. Add the service to the values, with the Service of the `Gateway` as the target:

   ```yaml
   serve:
     enabled: true
     services:
       - name: apps            # the serve service *.apps, without "*."
         services: "web=443@<gateway service>.<gateway namespace>.svc:80"
   ```

4. Make one `HTTPRoute` for each app, with `hostnames: ["<app>.apps.<serve zone>"]` and `parentRefs` to the `Gateway`.

A tailnet client then opens `https://<app>.apps.<serve zone>/`. The serve proxy gets one certificate for
`*.apps.<serve zone>` (see "Serve certificates") and terminates TLS. It sends plain HTTP to the `Gateway`, which
routes by the `Host` header and the path.

- A wildcard covers one label: `grafana.apps.<serve zone>` works, `a.b.apps.<serve zone>` does not.
- A new app needs only a new `HTTPRoute`. It needs no new serve service, no key and no `helm upgrade`.
- Access control is the headscale policy for the whole wildcard name. Per-app access rules go into the `Gateway`
  implementation.
- A controller that makes serve proxies from `Gateway` objects is proposed in #338.

### Serve certificates

`serve.certificate.source` sets where the proxies get their TLS certificate (#334):

| Source | What it does |
|---|---|
| `node-control` (default) | The proxy asks node-control for its certificate over the tailnet (`serve.url`). node-control signs it with its CA, or gives a public ACME certificate when it has `SERVE_CERT_SOURCE=acme`. This works on each cluster and on compose. |
| `cert-manager` | The chart makes one cert-manager `Certificate` for each proxy, with your `Issuer` or `ClusterIssuer`. The proxy reads the Secret of the Certificate and serves a renewed certificate with no restart. node-control is not used for certificates, and needs no DNS credentials. |

To use cert-manager:

1. Install cert-manager in the cluster. Make an `Issuer` or `ClusterIssuer` for the serve zone. An example is ACME with a DNS-01 solver and workload identity.
2. Set the values:

   ```yaml
   serve:
     certificate:
       source: cert-manager
       zone: svc.example.com
       issuerRef:
         name: <your-cluster-issuer>
         kind: ClusterIssuer
     services:
       - name: grafana                 # certificate for grafana.svc.example.com
         services: "web=443@grafana.monitoring.svc:3000"
       - name: apps                    # a wildcard service needs its own names
         services: "web=443@ingress-nginx-controller.ingress-nginx.svc:80"
         dnsNames: ["*.apps.svc.example.com"]
   ```

3. Run `helm upgrade`.
4. Check that each Certificate is ready: `kubectl -n tailnet get certificate`.

The names on the certificate must be the name of the service in node-control. The chart cannot check this. A wrong name gives a certificate error on the devices.

**NOTE:** The `Certificate` is a cert-manager resource. `helm install` fails if cert-manager is not installed in the cluster.

## Test

```sh
make test-k8s-connector      # helm lint, helm template and kubeconform, with the default and the example values, and both serve certificate sources
make test-k8s-connector-e2e  # kind and headscale (HEADSCALE_VERSION): join, route approval, a ClusterIP from the tailnet
```

The first target uses `helm` and `kubeconform` if you have them. If not, it uses their Docker images. The e2e test needs Docker, `kind`, `kubectl` and `helm`.

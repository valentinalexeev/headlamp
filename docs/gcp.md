# headlamp on Google Cloud: what works and what is missing

This page answers #40: how to run the central nodes of headlamp on Google Cloud, and what is missing. Each part has a
status: **works** (no GCP-specific code is needed), **works with settings**, or **gap** (code or a module is
missing). The gaps are at the end, as a list of follow-up work.

## Summary

| Part | On GCP today | Status |
|---|---|---|
| Control host (headscale, node-control, funnel-ingress, edge) | The compose example on a GCE VM | works with settings; no Terraform module (gap 1) |
| Exit nodes | `terraform/modules/exit-node-vm/gcp` and the node-control `gcp` provider (#214) | works |
| Funnel wildcard DNS record | The compose edge keeps it with `FUNNEL_DDNS=googleclouddns` | works with settings |
| Funnel and serve certificates (DNS-01) | Caddy edge and node-control ACME with `googleclouddns` / `googlecloud` | works with settings |
| GKE subnet router and serve proxies | `charts/tailnet-connector` | works |
| GKE Gateway API | `charts/gateway-controller` (#338) | works |
| Secrets for node-control | `gcpsm://` references read from Secret Manager at start (#361) | works with settings |
| node-control state | SQLite on a disk, or Redis (Memorystore) | works with settings |
| Task runner, bridges | Ansible on any Ubuntu or Debian VM | works |

## Control host

The compose example ([`examples/compose/control`](../examples/compose/control/README.md)) runs on any Linux host with
Docker, and so on a GCE VM. What to set:

- **VM.** `e2-small` or larger, Ubuntu 26.04 LTS, Docker and the compose plugin. Put the data directory on a separate
  persistent disk, with a snapshot schedule: it holds the tailnet.
- **Network.** Ports 443/tcp, 80/tcp and 3478/udp to the VM. A static external IP on the VM is enough. For a load
  balancer, use an external **passthrough** network load balancer (TCP and UDP, no TLS). Then the edge on the VM keeps
  the TLS certificates and the client address.
- **Admin access.** Use OS Login or IAP TCP forwarding for a shell. Open no SSH port to the internet.
- **Certificates.** The edge (Caddy) gets them from Let's Encrypt with HTTP-01. For funnel wildcards, use DNS-01 with
  the `googleclouddns` plugin (see "The edge and its certificates" in the compose README). On a GCE VM, the plugin uses
  the VM's service account: give it `roles/dns.admin` on the one managed zone only.

**Gap 1:** there is no `terraform/modules/control-host/gcp`, the twin of `control-host/aws`. It would make the VM, the
data disk with snapshots, a static IP or load balancer, the firewall and a service account for Cloud DNS. Today a site
writes this itself.

## Exit nodes

[`terraform/modules/exit-node-vm/gcp`](../terraform/modules/exit-node-vm/gcp/README.md) makes one VM that keeps its
headscale identity across stop and start, and node-control's `gcp` provider starts and stops it (#214). A Spot VM is
stopped, not deleted, on preemption. See "GCP permissions" in the node-control README for the role, the per-VM grant
and Workload Identity Federation from a control host outside GCP.

## Funnel

`funnel-ingress` runs on the control host behind the edge, as in the compose example. Two parts touch the DNS zone:

- **Certificates:** DNS-01 with `googleclouddns` (Caddy) works today.
- **The wildcard record:** the edge keeps `*.<FUNNEL_DOMAIN>` with `FUNNEL_DDNS=googleclouddns` and the same service
  account (see "The funnel record" in the compose README). Set `FUNNEL_DNS_CHECK=lookup` in node-control, and the
  Funnel page shows the status of the record.

## Serve names (tailnet-only HTTPS)

node-control can get the serve certificates from Let's Encrypt with DNS-01 on Cloud DNS: `SERVE_CERT_SOURCE=acme` and
`SERVE_ACME_DNS=googlecloud` (#334). On GCE it uses the VM's service account; elsewhere, Application Default
Credentials. On GKE, the `tailnet-connector` chart can use cert-manager instead (`serve.certificate.source:
cert-manager`), with a cert-manager Cloud DNS issuer and Workload Identity.

## GKE

- **Subnet router and serve proxies:** `charts/tailnet-connector` runs on GKE as on any cluster. It keeps the tailnet
  state in a Kubernetes Secret, so a pod restart keeps the node.
- **Gateway API:** `charts/gateway-controller` (#338) publishes Services from `Gateway` objects of class
  `headlamp-serve` or `headlamp-funnel`. GKE's own Gateway classes (`gke-l7-*`) do not change: the classes live side
  by side.
- **Workload Identity:** none of the charts needs a Google API, so no Workload Identity binding is needed. Only
  cert-manager (for serve certificates) does.

## Secrets and state

- **Secrets.** A secret variable of node-control can be a Secret Manager reference,
  `gcpsm://projects/<project>/secrets/<secret>`, read at start with the VM's service account
  (`roles/secretmanager.secretAccessor` on each secret). See "Secrets from Google Secret Manager" in the node-control
  README.
- **State.** SQLite on the data disk works. Memorystore for Redis works for the modules that use `REDIS_URL`. Put it
  on the VPC of the control host. With in-transit encryption on, use a `rediss://` URL and set `REDIS_CA_FILE` to the
  server CA of the instance. node-control also encrypts the state itself (`NODE_CONTROL_STATE_KEY`).

**Gap 3 (closed in #361):** before 0.21.0, a boot script had to copy the secrets out of Secret Manager.

## Task runner and bridges

The Ansible roles (`task-runner`, `reality-ingress`, `tailscale`) run on any Ubuntu or Debian VM, so they run on GCE.
Open the bridge ports in a GCE firewall rule by network tag. For a 1 vCPU / 1 GB VM, see #215.

## Follow-up work

| Gap | Size | Issue |
|---|---|---|
| 1. `terraform/modules/control-host/gcp` | ~2 days | #359 |
| 2. Cloud DNS provider for the funnel wildcard record | done: the edge keeps it | #384 |
| 3. Secret Manager reader for node-control secrets (and a Redis CA setting) | done | #361 |

Nothing else blocks a deployment on GCP: the exit nodes, the funnel and serve certificates, and GKE work today.

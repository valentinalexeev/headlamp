# headlamp on Google Cloud: what works and what is missing

This page answers #40: how to run the central nodes of headlamp on Google Cloud, and what is missing. Each part has a
status: **works** (no GCP-specific code is needed), **works with settings**, or **gap** (code or a module is
missing). The gaps are at the end, as a list of follow-up work.

## Summary

| Part | On GCP today | Status |
|---|---|---|
| Control host (headscale, node-control, funnel-ingress, edge) | The compose example on a GCE VM | works with settings; no Terraform module (gap 1) |
| Exit nodes | `terraform/modules/exit-node-vm/gcp` and the node-control `gcp` provider (#214) | works |
| Funnel wildcard DNS record | node-control writes it for Route 53 and Cloudflare only | gap 2 |
| Funnel and serve certificates (DNS-01) | Caddy edge and node-control ACME with `googleclouddns` / `googlecloud` | works with settings |
| GKE subnet router and serve proxies | `charts/tailnet-connector` | works |
| GKE Gateway API | `charts/gateway-controller` (#338) | works |
| Secrets for node-control | Environment and files; no Secret Manager reader | works with settings (gap 3) |
| node-control state | SQLite on a disk, or Redis (Memorystore) | works with settings |
| Task runner, bridges | Ansible on any Ubuntu or Debian VM | works |

## Control host

The compose example ([`examples/compose/control`](../examples/compose/control/README.md)) runs on any Linux host with
Docker, and so on a GCE VM. What to set:

- **VM.** `e2-small` or larger, Ubuntu 26.04 LTS, Docker and the compose plugin. Put the data directory on a separate
  persistent disk, with a snapshot schedule: it holds the tailnet.
- **Network.** Ports 443/tcp, 80/tcp and 3478/udp to the VM. A static external IP on the VM is enough. For a load
  balancer, use an external **passthrough** network load balancer (TCP and UDP, no TLS), so the edge on the VM keeps
  the TLS certificates and the client address.
- **Admin access.** Use OS Login or IAP TCP forwarding for a shell. Open no SSH port to the internet.
- **Certificates.** The edge (Caddy) gets them from Let's Encrypt with HTTP-01. For funnel wildcards, use DNS-01 with
  the `googleclouddns` plugin (see "The edge and its certificates" in the compose README). On a GCE VM, the plugin uses
  the VM's service account: give it `roles/dns.admin` on the one managed zone only.

**Gap 1:** there is no `terraform/modules/control-host/gcp`, the twin of `control-host/aws` (VM, data disk with
snapshots, static IP or load balancer, firewall, service account for Cloud DNS). Today a site writes this itself.

## Exit nodes

[`terraform/modules/exit-node-vm/gcp`](../terraform/modules/exit-node-vm/gcp/README.md) makes one VM that keeps its
headscale identity across stop and start, and node-control's `gcp` provider starts and stops it (#214). A Spot VM is
stopped, not deleted, on preemption. See "GCP permissions" in the node-control README for the role, the per-VM grant
and Workload Identity Federation from a control host outside GCP.

## Funnel

`funnel-ingress` runs on the control host behind the edge, as in the compose example. Two parts touch the DNS zone:

- **Certificates:** DNS-01 with `googleclouddns` (Caddy) works today.
- **The wildcard record:** node-control writes `*.<namespace>.<FUNNEL_DOMAIN>` for `FUNNEL_DNS_PROVIDER=route53` or
  `cloudflare`. With a Cloud DNS zone, the Funnel page shows a checklist, and you add the record by hand.

**Gap 2:** a `googleclouddns` provider for the funnel record in `internal/dnsrec` (Cloud DNS `changes.create`, with the
same service account as the edge).

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

- **Secrets.** node-control reads its secrets from the environment and from files. On GCE, keep them in Secret
  Manager and write them to the compose `.env` or secret files at boot, as the exit node module does for its auth key.
- **State.** SQLite on the data disk works. For the modules that use Redis (`REDIS_URL`), Memorystore for Redis works:
  put it on the VPC of the control host, with in-transit encryption off. node-control encrypts the state itself
  (`NODE_CONTROL_STATE_KEY`). With in-transit encryption on, Memorystore uses a server CA that node-control has no
  setting for yet.

**Gap 3 (small):** an optional Secret Manager reader in node-control, so a GCE host needs no boot script for secrets.
This is a convenience, not a blocker.

## Task runner and bridges

The Ansible roles (`task-runner`, `reality-ingress`, `tailscale`) run on any Ubuntu or Debian VM, so they run on GCE.
Open the bridge ports in a GCE firewall rule by network tag. For a 1 vCPU / 1 GB VM, see #215.

## Follow-up work

| Gap | Size | Issue |
|---|---|---|
| 1. `terraform/modules/control-host/gcp` | ~2 days | #359 |
| 2. Cloud DNS provider for the funnel wildcard record | ~1 day | #360 |
| 3. Secret Manager reader for node-control secrets (and a Redis CA setting) | ~1 day, optional | #361 |

Nothing else blocks a deployment on GCP: the exit nodes, the funnel and serve certificates, and GKE work today.

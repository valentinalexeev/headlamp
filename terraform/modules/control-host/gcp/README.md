# control-host/gcp

This module makes one GCE host for the central nodes of headlamp: `headscale`, `node-control` and `funnel-ingress`.
They run with Docker Compose, from [`examples/compose/control`](../../../../examples/compose/control/). That README
gives the full procedure. It is the twin of [`control-host/aws`](../aws/) (#359).

## What it makes

- One VM: Ubuntu 26.04 LTS Minimal, Docker and the compose plugin, Shielded VM, OS Login, no project SSH keys.
- A separate persistent data disk, mounted at `/srv/headlamp/data`. The module formats it only when it is empty, so a
  new VM keeps the tailnet. The disk has `prevent_destroy`.
- Daily snapshots of the data disk (`snapshot_retain_days`, default 14, `0` turns them off). The snapshots stay when
  the disk is deleted.
- A static external address. By default it is on the VM. With `load_balancer = true`, an external passthrough network
  load balancer holds it and sends 443/tcp, 80/tcp and 3478/udp to the VM. It does not end TLS, and it keeps the
  client address.
- A firewall rule with these three ports, for the host's network tag. SSH is closed unless you set `admin_cidrs`.
- A service account with no project role. With `dns_zone`, it gets `roles/dns.admin` on that one managed zone, for
  ACME DNS-01 and the funnel record of node-control (`FUNNEL_DNS_PROVIDER=googleclouddns`). With `secrets`, it can read
  each listed secret, for `gcpsm://` references of node-control.

## Inputs

| Name | Default | What it does |
|---|---|---|
| `name` | required | The name prefix of all resources (lower case letters, digits and `-`) |
| `project`, `region`, `zone` | required | Where the host runs. The zone must be in the region. |
| `machine_type` | `e2-small` | The machine type |
| `image` | `ubuntu-os-cloud/ubuntu-minimal-2604-lts-amd64` | The boot image. It must run cloud-init. |
| `boot_disk_gb`, `data_disk_gb` | `20`, `10` | Disk sizes |
| `data_disk_type` | `pd-balanced` | The type of the data disk |
| `snapshot_retain_days` | `14` | The number of days to keep daily snapshots |
| `data_snapshot` | none | Make the data disk from this snapshot, for a restore |
| `network`, `subnetwork` | `default`, none | The VPC network and, for a custom mode network, the subnetwork |
| `load_balancer` | `false` | A passthrough network load balancer, or the static address on the VM |
| `admin_cidrs` | none | Addresses that can use SSH. For IAP, `35.235.240.0/20`. |
| `dns_zone`, `dns_zone_project` | none, `project` | The managed zone for ACME DNS-01 and the funnel record |
| `secrets` | none | Secrets the host may read, as `projects/<p>/secrets/<s>` |
| `user_data_extra` | none | Shell commands that cloud-init runs at the end |
| `labels` | none | Labels for all resources that take them |

The VM ignores changes of `image`, so a new image does not replace a running host. A change of `user_data_extra`
changes the metadata in place. cloud-init runs it only on a new VM.

## Shell access

The host has OS Login and no public SSH port. Open a shell through IAP:

1. Set `admin_cidrs = ["35.235.240.0/20"]` (the IAP range).
2. Give your user `roles/iap.tunnelResourceAccessor` and `roles/compute.osAdminLogin`.
3. Run `gcloud compute ssh <instance_name> --zone <zone> --tunnel-through-iap`.

Or use the tailnet, once the host is on it.

## Outputs

`public_address` (point your DNS names here, A records), `instance_name`, `data_disk`, `service_account_email` and
`network_tag`.

## Test

`make test-control-host-gcp` runs plan checks with a mock provider. It needs no GCP project. The module is not yet
tested against a real project.

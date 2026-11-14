# exit-node-vm/gcp

This module makes one exit node on Google Compute Engine (#214). The node joins your tailnet with the auth key that you
give. node-control's `gcp` provider starts and stops it.

## Node identity across stop and start

The module makes one VM, not a managed instance group. A stop keeps the boot disk, and tailscaled keeps its state on
that disk. So a start brings back the **same** headscale node: the same id, name and IPs. The node does not use the auth
key again. An instance group would delete the VM and its disk at size 0, and a new node would register each time
(#213 on AWS).

A Spot VM (the default, `spot = true`) is **stopped, not deleted**, when GCE takes it back. node-control shows it as
stopped. You can start it again, and then it is the same node. `spot = false` makes a standard VM that restarts by
itself after a host event.

The external IPv4 address is ephemeral. It can change on a stop and start. The tailnet identity does not change.

## Auth key

Read the key from Secret Manager, so that it is not in the Terraform state or the instance metadata:

```hcl
resource "google_secret_manager_secret" "authkey" {
  project   = "<project>"
  secret_id = "cobalt-authkey"
  replication {
    auto {}
  }
}

module "cobalt" {
  source                 = "../vendor/headlamp/terraform/modules/exit-node-vm/gcp"
  name                   = "cobalt"
  project                = "<project>"
  zone                   = "europe-west3-a"
  headscale_login_server = "https://<headscale host>"
  tailscale_version      = "1.104.5"
  authkey_from_secret    = true
  authkey_secret         = google_secret_manager_secret.authkey.id
}
```

Add the key as a secret version outside Terraform, for example with `gcloud secrets versions add cobalt-authkey
--data-file=-`. Then it is not in the state.

- `authkey_from_secret` is a plain bool, so the secret can be made in the same configuration (#223). The plan never
  depends on the secret's value.
- The node's own service account gets `roles/secretmanager.secretAccessor` on **that secret only**. It has no project
  role.
- At the first boot, cloud-init reads the latest version with a token from the metadata server. It writes the key to
  tmpfs (mode 0600), runs `tailscale up`, and deletes the file. The key is not in the metadata or the startup log.
- The key is needed only for the first registration. Use a reusable key only if you rebuild the VM often.

`tailscale_authkey` still works: it puts the key in the metadata, and so in the state. Set only one of the two.

## node-control

1. Tag the node `tag:cloud-gcp` in the headscale policy, or add a `"gcp"` block for it in `nodes.json`.
2. Give node-control the rights to start and stop this VM, and no other. Set `controller_member` and `controller_role`:
   the module grants the role on this VM only. See "GCP permissions" in the node-control README for the role and for
   the list permission.
3. The module puts the label `headlamp-exit-node=true` on the instance. node-control refuses a VM without it.

```hcl
module "cobalt" {
  # ...
  controller_member = "serviceAccount:node-control@<project>.iam.gserviceaccount.com"
  controller_role   = "projects/<project>/roles/headlampExitNodeControl"
}
```

## Labels

`labels` go on the instance **and** its boot disk (#226), so label-based cost reports and clean-up see both. The control
label `headlamp-exit-node` is on the instance only.

## Security

- Shielded VM: secure boot, vTPM and integrity monitoring.
- OS Login is on, and project-wide SSH keys are blocked. The firewall rule opens only WireGuard (`41641/udp`), by the
  network tag `headlamp-exit-node`. There is no SSH rule: admin access is over the tailnet.
- `can_ip_forward` is on, because an exit node forwards traffic for other hosts.

## Inputs

| Name | Default | Description |
|---|---|---|
| `name` | (required) | Instance name, and the tailscale hostname. node-control finds the VM by it. |
| `zone` | (required) | Zone, for example `europe-west3-a`. |
| `headscale_login_server` | (required) | URL of the headscale server. |
| `project` | provider's project | Project of the VM. |
| `tailscale_hostname` | `name` | Hostname on the tailnet. |
| `machine_type` | `e2-micro` | Machine type. |
| `spot` | `true` | Spot VM, stopped on preemption. |
| `image` | `ubuntu-os-cloud/ubuntu-minimal-2604-lts-amd64` | Boot image; it must run cloud-init. |
| `disk_size_gb` | `10` | Boot disk size. |
| `network`, `subnetwork` | `default`, null | Network and subnetwork. |
| `labels` | `{}` | Labels for the instance and its boot disk. |
| `service_account_id` | `<name>-exit-node` | Account id of the node's service account. |
| `tailscale_version` | null | Pinned tailscale version from the signed apt repository. Null: `install.sh`. |
| `authkey_from_secret`, `authkey_secret` | `false`, null | Read the auth key from this Secret Manager secret. |
| `tailscale_authkey` | null | The auth key in the metadata (prefer the secret). |
| `controller_member`, `controller_role` | null | Grant node-control this role on this VM only. |

## Outputs

`instance_name`, `zone`, `service_account_email`, `public_ip`.

## Tests

`make test-exit-node-gcp` runs the plan checks with a mocked provider (no project needed) and the secret fetcher
against a fake metadata server.

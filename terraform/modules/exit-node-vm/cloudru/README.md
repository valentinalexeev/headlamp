# exit-node-vm/cloudru

A tailnet exit node on a **Cloud.ru Evolution** VM (the former SberCloud). The module creates these resources: subnet,
security group (inbound only tailscale's UDP port, optional SSH), public IP, boot disk, network interface and the VM itself.

**Status:** schema-validated against provider `cloudru/cloud` v2.1.5 (`terraform validate`). It has **not been
applied against a live Cloud.ru account**. Check the first `terraform plan` and `terraform apply` carefully.
The Cloud.ru docs leave these points open:

- Whether `cloud_init_userdata` is base64. The module follows the Cloud.ru guide and encodes it in base64.
- Which `image_metadata` an image needs.
- The exact flavor and disk-type names in your zone.

## Provider

The provider is not on the public Terraform registry. Install it from the network mirror of Cloud.ru, or as a local
plugin ([quick start](https://cloud.ru/docs/terraform-evolution/ug/topics/quickstart)). Configure it in your root
stack. The credentials come from a service account key, for example through the `AUTH_KEY_ID` and `AUTH_SECRET` env vars:

```hcl
provider "cloudru" {
  project_id = var.cloudru_project_id
}
```

The schema of the provider changed between minor versions, so `versions.tf` pins `~> 2.1.5`.

## Usage

```hcl
module "node" {
  source         = "../vendor/headlamp/terraform/modules/exit-node-vm/cloudru"
  name           = "node-c"
  project_id     = var.cloudru_project_id
  zone_name      = "<zone>"                 # data "cloudru_evolution_compute_zone_collection"
  vpc_id         = var.cloudru_vpc_id       # an existing VPC
  ssh_public_key = var.ssh_public_key
  admin_ssh_cidr = "203.0.113.7/32"         # needed for the first Ansible run, see below
}
```

## Tailscale bootstrap

`bootstrap_tailscale` defaults to **false**. Hosts in restricted networks often cannot reach `tailscale.com` and
`pkgs.tailscale.com`, so cloud-init does not try. Instead, run the Ansible `tailscale` role against `module.node.external_ip`.
The role downloads the `.deb` on the control host and copies it over. That first run needs SSH from your admin address
(`admin_ssh_cidr`).

When the node is on the tailnet, you can drop `admin_ssh_cidr`. Set
`bootstrap_tailscale = true` (plus `headscale_login_server`, `tailscale_authkey`) only if the VM can reach those hosts.

**CAUTION:** `tailscale_authkey` is in the Terraform state and in the VM's user data, and any process on the VM can
read user data. Cloud.ru has no Barbican, so use a **single-use, short-lived** key, for example
`headscale preauthkeys create --user <user> --expiration 1h` (not `--reusable`). The disk stays when the VM stops and
starts. Thus the node keeps its identity and does not need the key again (#224).

## Preemptible VMs

As of v2.1.5, the Cloud.ru Evolution provider has no preemptible or spot option, so this node is a regular VM.

## Boot image

The default of `image_name` is `ubuntu-22.04`. The default stays because no public source confirms a Ubuntu 26.04
image in Cloud.ru Evolution. To find the images of your project, use the
`cloudru_evolution_compute_image_collection` data source. If your project has a 26.04 image, set `image_name` to its
name.

### Supported images

The table gives the default image and the override for each provider of `exit-node-vm`. All images must run
cloud-init. The Ansible roles are tested on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13 (#178).

| Provider | Default image | Override |
|---|---|---|
| `aws` | Ubuntu 26.04 Minimal (Canonical AMI) | `image_flavor = "server"` for Ubuntu Server. `ami_id` for a different AMI, for example Debian 13 |
| `oci` | Ubuntu 26.04 Minimal. If OCI has no such image, Ubuntu 24.04 Minimal | `ubuntu_version`, `ubuntu_version_fallback` |
| `yandex` | Ubuntu 26.04 LTS (family `ubuntu-2604-lts`) | `image_family` or `image_id`, for example family `debian-13` |
| `openstack`, `selectel` | `Ubuntu 26.04 LTS 64-bit` | `image_name`, or `image_id` to pin one image. Selectel: `Debian 13 (Trixie) 64-bit` (#235) |
| `cloudru` | `ubuntu-22.04` | `image_name` |

Ubuntu Minimal has fewer packages and services than Ubuntu Server, and it has no snapd. Yandex Cloud, Selectel and
Cloud.ru do not publish a Minimal image, so these modules use the server image.

## Replacing the VM

Terraform ignores `cloud_init_userdata` after creation, because it runs only on first boot. Redeploy on purpose with
`terraform apply -replace='module.<name>.cloudru_evolution_compute_vm.this'`.

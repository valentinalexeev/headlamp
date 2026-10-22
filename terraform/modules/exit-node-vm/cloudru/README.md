# exit-node-vm/cloudru

A tailnet exit node on a **Cloud.ru Evolution** VM (the former SberCloud): subnet, security group (inbound only
tailscale's UDP port, optional SSH), public IP, boot disk, network interface and the VM itself.

**Status:** schema-validated against provider `cloudru/cloud` v2.1.5 (`terraform validate`). It has **not been
applied against a live Cloud.ru account**, so check the first `terraform plan`/`apply` carefully. Points the docs
leave open: whether `cloud_init_userdata` is base64 (it follows Cloud.ru's guide and base64-encodes), which
`image_metadata` an image needs, and the exact flavor/disk-type names in your zone.

## Provider

The provider is not on the public Terraform registry. Install it through Cloud.ru's network mirror or as a local
plugin ([quick start](https://cloud.ru/docs/terraform-evolution/ug/topics/quickstart)) and configure it in your root
stack (credentials from a service account key, e.g. via the `AUTH_KEY_ID` / `AUTH_SECRET` env vars):

```hcl
provider "cloudru" {
  project_id = var.cloudru_project_id
}
```

The provider's schema changed between minor versions, so `versions.tf` pins `~> 2.1.5`.

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

`bootstrap_tailscale` defaults to **false**: hosts in restricted networks often cannot reach `tailscale.com` /
`pkgs.tailscale.com`, so cloud-init does not try. Instead run the Ansible `tailscale` role (it downloads the `.deb`
on the control host and copies it over) against `module.node.external_ip`, which needs SSH from your admin address
(`admin_ssh_cidr`) for that first run. Once the node is on the tailnet you can drop `admin_ssh_cidr`. Set
`bootstrap_tailscale = true` (plus `headscale_login_server`, `tailscale_authkey`) only if the VM can reach those hosts.

## Preemptible VMs

Cloud.ru Evolution's provider exposes no preemptible/spot option as of v2.1.5, so this node is a regular VM.

## Replacing the VM

`cloud_init_userdata` is ignored after creation (it only runs on first boot). Redeploy deliberately with
`terraform apply -replace='module.<name>.cloudru_evolution_compute_vm.this'`.

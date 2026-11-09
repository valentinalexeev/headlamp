# exit-node-vm/yandex

This module makes one exit node VM in Yandex Compute. The VM joins your tailnet with the auth key that you give.

The module has no `provider` block. It uses the `yandex` provider of your root stack. Thus the root stack selects the
cloud installation, the credentials and the folder.

## Inputs

See `variables.tf`. The most important inputs are `name`, `folder_id`, `zone`, `subnet_id`, `image_family` or `image_id`, the sizing
inputs, `headscale_login_server` and `tailscale_authkey`.

## Boot image

The module uses the newest image of the public family in `image_family`. The default is `ubuntu-2604-lts` (Ubuntu
26.04 LTS). When you set `image_id`, the module uses that image and ignores `image_family`.

Terraform ignores a change of the image after the VM exists. Thus a new default does not replace a running VM. To make
new VMs with the old image, set `image_id` or `image_family = "ubuntu-2404-lts"` in your root stack. To make a new VM
with the new image, run `terraform apply -replace='module.<name>.yandex_compute_instance.this'`.

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

## Use a separate cloud installation

A separate installation of the cloud has its own API endpoint, console and billing account. To make a node there, do
these steps in your private root stack:

1. Declare a provider with an alias, and set its `endpoint` to the API endpoint of the installation:

   ```hcl
   provider "yandex" {
     alias                    = "other"
     endpoint                 = "api.<installation-domain>:443"
     service_account_key_file = var.other_sa_key_file
     folder_id                = var.other_folder_id
   }
   ```

2. Give that provider to the module:

   ```hcl
   module "node2" {
     source    = "../vendor/headlamp/terraform/modules/exit-node-vm/yandex"
     providers = { yandex = yandex.other }
     zone      = "<zone of that installation>"
     # the other inputs as usual
   }
   ```

3. Use the zone, subnet and image IDs of that installation. IDs of one installation do not work in a different one.

To start and stop that node from node-control, set its API endpoints in `nodes.json`. See "Optional overrides" in
`apps/node-control/README.md`.

# exit-node-vm/aws

This module makes one exit node on AWS EC2. An auto scaling group with a size of one runs the node. The node joins your
tailnet with the auth key that you give.

## Boot image

The module uses the newest Canonical AMI of the Ubuntu series in `ubuntu_series` and the flavor in `image_flavor`. The
default series is `resolute-26.04` (Ubuntu 26.04 LTS). The default flavor is `minimal` (Ubuntu Minimal). To use the
full Ubuntu Server image, set `image_flavor = "server"`. To use a different AMI, set `ami_id`. The AMI must run
cloud-init.

A change of the series makes a new version of the launch template. The running instance stays. The group uses the new
image only when it starts a new instance, for example after a stop and start by node-control.

To keep the old image on all new instances, set the series in your root stack:

```hcl
module "node" {
  source        = "../vendor/headlamp/terraform/modules/exit-node-vm/aws"
  ubuntu_series = "noble-24.04"
  # the other inputs as usual
}
```

### Supported images

The table gives the default image and the override for each provider of `exit-node-vm`. All images must run
cloud-init. The Ansible roles are tested on Ubuntu only.

| Provider | Default image | Override |
|---|---|---|
| `aws` | Ubuntu 26.04 Minimal (Canonical AMI) | `image_flavor = "server"` for Ubuntu Server. `ami_id` for a different AMI, for example Debian 13 |
| `oci` | Ubuntu 26.04 Minimal. If OCI has no such image, Ubuntu 24.04 Minimal | `ubuntu_version`, `ubuntu_version_fallback` |
| `yandex` | Ubuntu 26.04 LTS (family `ubuntu-2604-lts`) | `image_family` or `image_id`, for example family `debian-13` |
| `openstack`, `selectel` | `Ubuntu 26.04 LTS 64-bit` | `image_name` |
| `cloudru` | `ubuntu-22.04` | `image_name` |

Ubuntu Minimal has fewer packages and services than Ubuntu Server, and it has no snapd. Yandex Cloud, Selectel and
Cloud.ru do not publish a Minimal image, so these modules use the server image.

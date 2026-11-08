# exit-node-vm/oci

This module makes one exit node on Oracle Cloud Infrastructure (OCI). The node joins your tailnet with the auth key
that you give.

## Boot image

The module uses the newest "Canonical Ubuntu" platform image of the version in `ubuntu_version`. The default is
`26.04 Minimal`. If OCI has no such image for the shape, the module uses the version in `ubuntu_version_fallback`.
The default fallback is `24.04 Minimal`. If no image of the two versions exists, the plan stops with an error.

To turn
off the fallback, set `ubuntu_version_fallback = null`. To use the full server image, set `ubuntu_version = "24.04"`.

Terraform ignores a change of the image after the instance exists. Thus a change of `ubuntu_version` does not replace
a running node. To make a new node with the new image, run
`terraform apply -replace='module.<name>.oci_core_instance.this'`.

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

# exit-node-vm/selectel

This is the old name of [`exit-node-vm/openstack`](../openstack/README.md). It calls that module with the same inputs
and outputs.

**Deprecated.** This module will be removed by 1.0.0. Move your root stack to `exit-node-vm/openstack`.

Existing root stacks continue to work. The `moved` blocks in `main.tf` move the state into the new module, so the
first plan after the upgrade shows no change. For a new root stack, use `exit-node-vm/openstack`.

The default of `image_name` is `Ubuntu 26.04 LTS 64-bit`. To keep the old image, set
`image_name = "Ubuntu 22.04 LTS 64-bit"`. See [Boot image](../openstack/README.md#boot-image).

## Boot image

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

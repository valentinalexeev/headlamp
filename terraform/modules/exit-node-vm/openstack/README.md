# exit-node-vm/openstack

A tailnet exit node on an **OpenStack** cloud server, for example Selectel or another OpenStack-compatible cloud. The
module creates these resources:

- key pair
- private network and router
- security group (inbound only tailscale's UDP port, optional SSH)
- port
- bootable network volume
- the server and a floating IP

**Status:** schema-validated against `terraform-provider-openstack/openstack` v3.4.0 (`terraform validate`). It has
**not been applied against a live account**. Read the first `plan` carefully. Flavor IDs, volume types and image names
differ for each cloud and pool. The defaults (`external_network_name`, `image_name`) are the Selectel names.

The module `exit-node-vm/selectel` is the old name of this module. It continues to work for existing root stacks. It
calls this module and moves the state for you (`moved` blocks). Thus the first plan after the upgrade shows no change.

## Provider

The module needs only the `openstack` provider. Configure it in your root stack. For Selectel, configure it against
the Keystone of Selectel. Use a **service user** that is a member of your project. Create the project and the user in the control
panel, or with `selectel_vpc_project_v2` and `selectel_iam_serviceuser_v1` of the `selectel` provider:

```hcl
provider "openstack" {
  auth_url    = "https://cloud.api.selcloud.ru/identity/v3"
  domain_name = var.selectel_account_id     # the account number
  tenant_id   = var.selectel_project_id
  user_name   = var.selectel_service_user
  password    = var.selectel_service_password   # keep it in sops / env, never in git
  region      = "<region>"
}
```

## Usage

```hcl
module "node" {
  source            = "../vendor/headlamp/terraform/modules/exit-node-vm/openstack"
  name              = "node-b"
  availability_zone = "<zone>"
  flavor_id         = "<fixed-configuration flavor id>"   # see Selectel's flavor table
  volume_type       = "<volume-type>"
  ssh_public_key    = var.ssh_public_key
  admin_ssh_cidr    = "203.0.113.7/32"                    # needed for the first Ansible run, see below
}
```

## Other OpenStack-compatible clouds

The module uses only the `openstack` provider. Thus it works on other clouds that run OpenStack with their own Keystone.
Do these steps in your private root stack:

1. Declare an `openstack` provider with an alias, and set its `auth_url`, `region` and `domain_name` for that cloud:

   ```hcl
   provider "openstack" {
     alias       = "other"
     auth_url    = "https://keystone.<other-cloud-domain>/v3"
     domain_name = var.other_domain
     tenant_id   = var.other_project_id
     user_name   = var.other_user
     password    = var.other_password    # keep it in sops / env, never in git
     region      = "<region>"
   }
   ```

2. Give that provider to the module, with the names and IDs of that cloud:

   ```hcl
   module "node2" {
     source                = "../vendor/headlamp/terraform/modules/exit-node-vm/openstack"
     providers             = { openstack = openstack.other }
     availability_zone     = "<zone>"
     flavor_id             = "<flavor id>"
     image_name            = "<public Ubuntu image name of that cloud>"
     volume_type           = "<volume type>"
     external_network_name = "<external network>"   # openstack network list --external
     # the other inputs as usual
   }
   ```

3. Check the first `plan` carefully. Flavors, volume types, images and the external network differ for each cloud.

**CAUTION:** do not set `preemptible = true` on a different cloud. The `preemptible` tag has a meaning only for Selectel.

node-control can start and stop the node. Declare the cloud in `OPENSTACK_CLOUDS`, and name it in the block of the node.
See "Optional overrides" in
`apps/node-control/README.md`.

## Outputs

- `instance_id`: the server's ID
- `external_ip`: the floating IP
- `security_group_id`: the node's security group, so a root stack can add rules (for example a proxy port) with `openstack_networking_secgroup_rule_v2`
- `image_id`: the image of the boot volume

## Tags and metadata

Use `tags` and `metadata` to find and bill the node's resources by site (#227):

- `tags` (list): the instance, port, network, subnet, router, security group and floating IP. The instance also
  gets `preemptible` when `preemptible = true`.
- `metadata` (map): the instance and the boot volume. Cinder volumes have no tags.

```hcl
  tags     = ["site-a"]
  metadata = { project = "site-a" }
```

```sh
openstack port list --tags site-a
openstack volume list --property project=site-a
```

With the defaults (`[]` and `{}`) the module sets no tags and no metadata, so an existing stack plans no change.
A stack that adds tags updates the resources in place. It does not replace them.

## Bridge node

A bridge (`reality-ingress`, `ingress-agent`, `tailscale-userspace-socks`) uses this module too (#235). It needs public
ports that an exit node does not need, and it must not advertise exit routes:

| Input | Exit node | Bridge |
|---|---|---|
| `extra_tcp_ingress_ports` | `[]` | `[80, 443]`: ACME HTTP-01, and Reality |
| `extra_udp_ingress_ports` | `[]` | `[443]`: Hysteria 2 (`hy2_listen_port`). Leave it out with `hy2_enabled: false` |
| `advertise_exit_node` | `true` | `false` |
| `bootstrap_tailscale` | `false` (recommended) | `false`: the Ansible `tailscale` role joins it, with the bridge tag |

Port 22 is not allowed in `extra_tcp_ingress_ports`. SSH is only from `admin_ssh_cidr`.

```hcl
module "bridge_spb" {
  source                  = "../vendor/headlamp/terraform/modules/exit-node-vm/openstack"
  name                    = "bridge-spb"
  availability_zone       = "<zone>"
  flavor_id               = "<fixed-configuration flavor id>"
  volume_type             = "<volume-type>"
  image_name              = "Debian 13 (Trixie) 64-bit"   # the Selectel name; or image_id to pin one image
  ssh_public_key          = var.ssh_public_key
  admin_ssh_cidr          = "203.0.113.7/32"              # for the first Ansible run
  extra_tcp_ingress_ports = [80, 443]
  extra_udp_ingress_ports = [443]
  advertise_exit_node     = false
}
```

Then run the roles against the bridge: `tailscale` (with `tailscale_advertise_exit_node: false` and the bridge tag),
`tailscale-userspace-socks`, `reality-ingress` and `ingress-agent`. Read the HARD GATE in the `reality-ingress` README
first: the bridge tag must reach only its exit node. `examples/private-repo` has a bridge host in the inventory, its
`host_vars` and the play.

### Debian 13

Selectel has the public image `Debian 13 (Trixie) 64-bit` (checked 2026-11-09). Check that your pool has it. Two things differ from Ubuntu:

- The login user is `debian`, not `ubuntu`. Set `ansible_user: debian` in the inventory.
- The image is smaller. It has no cron and no `procps` at first. The roles install what they need (#178).

`image_name` takes the newest image with that name at each plan. Terraform ignores an image change on a running server,
but a replaced server gets the newest image. To build each server from the same image, set `image_id` (from
`openstack image show "Debian 13 (Trixie) 64-bit" -f value -c id`).

## Tailscale bootstrap

`bootstrap_tailscale` defaults to **false**. Hosts in restricted networks often cannot reach `tailscale.com` and `pkgs.tailscale.com`,
so cloud-init does not try. Run the Ansible `tailscale` role instead against `module.node.external_ip`.
The role downloads the `.deb` on the control host and copies it over. That first run needs SSH from `admin_ssh_cidr`.
You can drop `admin_ssh_cidr` when the node is on the tailnet.

## Preemptible servers

Selectel offers preemptible servers (about 70% cheaper, no SLA). Selectel **stops these servers at the latest 24 hours after creation**.
The server goes to `EXPIRED`. Network volumes are kept. Restore the server with `openstack server unshelve <server>`.
The server stays preemptible.

For this reason, they are a poor fit for a standing exit node, unless you automate the restore.

`preemptible = true` adds the `preemptible` server tag. Selectel documents the tag with compute API microversion
2.72. The OpenStack provider requests 2.52 for tags. After the first apply, **check that the tag exists**:

```bash
openstack server show <name> -c tags --os-compute-api-version 2.72
# if it is missing:
openstack server set --tag preemptible --os-compute-api-version 2.72 <name>
```

You can switch an existing server to preemptible once in 24 hours.

## Boot image

The default of `image_name` is `Ubuntu 26.04 LTS 64-bit`, the Selectel name of Ubuntu 26.04 LTS. Other clouds use
different image names. Set `image_name` to the name of your cloud.

Terraform ignores a change of the image after the server exists. Thus a new default does not replace a running server.
To make new servers with the old image, set `image_name = "Ubuntu 22.04 LTS 64-bit"` in your root stack.

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

## Replacing the server

Terraform ignores `user_data` after creation, because it runs only on first boot. Redeploy on purpose with
`terraform apply -replace='module.<name>.openstack_compute_instance_v2.this'`.

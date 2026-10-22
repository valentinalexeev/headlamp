# exit-node-vm/selectel

A tailnet exit node on a **Selectel** cloud server (OpenStack): key pair, private network + router, security group
(inbound only tailscale's UDP port, optional SSH), port, bootable network volume, the server and a floating IP.

**Status:** schema-validated against `terraform-provider-openstack/openstack` v3.4.0 (`terraform validate`). It has
**not been applied against a live Selectel account**, so read the first `plan` carefully (flavor IDs, volume types and
image names differ per pool).

## Provider

Selectel's cloud is OpenStack, so the module only needs the `openstack` provider, configured in your root stack against
Selectel's Keystone with a **service user** that is a member of your project (create the project and user in the control
panel or with the `selectel` provider's `selectel_vpc_project_v2` / `selectel_iam_serviceuser_v1`):

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
  source            = "../vendor/headlamp/terraform/modules/exit-node-vm/selectel"
  name              = "node-b"
  availability_zone = "<zone>"
  flavor_id         = "<fixed-configuration flavor id>"   # see Selectel's flavor table
  volume_type       = "<volume-type>"
  ssh_public_key    = var.ssh_public_key
  admin_ssh_cidr    = "203.0.113.7/32"                    # needed for the first Ansible run, see below
}
```

## Outputs

- `instance_id`: the server's ID
- `external_ip`: the floating IP
- `security_group_id`: the node's security group, so a root stack can add rules (for example a proxy port) with `openstack_networking_secgroup_rule_v2`

## Tailscale bootstrap

`bootstrap_tailscale` defaults to **false**: hosts in restricted networks often cannot reach `tailscale.com` / `pkgs.tailscale.com`,
so cloud-init does not try. Run the Ansible `tailscale` role instead (it downloads the `.deb` on the control host and
copies it over) against `module.node.external_ip`; that first run needs SSH from `admin_ssh_cidr`, which you can drop once
the node is on the tailnet.

## Preemptible servers

Selectel offers preemptible servers (about 70% cheaper, no SLA) that are **stopped at the latest 24 hours after creation**:
the server goes to `EXPIRED`, network volumes are kept, and you restore it with `openstack server unshelve <server>` (it
stays preemptible). That makes them a poor fit for a standing exit node unless you automate the restore.

`preemptible = true` adds the `preemptible` server tag. Selectel documents the tag together with compute API microversion
2.72, while the OpenStack provider requests 2.52 for tags, so **check that the tag took** after the first apply:

```bash
openstack server show <name> -c tags --os-compute-api-version 2.72
# if it is missing:
openstack server set --tag preemptible --os-compute-api-version 2.72 <name>
```

(An existing server can be switched to preemptible once per 24 hours.)

## Replacing the server

`user_data` is ignored after creation (it only runs on first boot). Redeploy deliberately with
`terraform apply -replace='module.<name>.openstack_compute_instance_v2.this'`.

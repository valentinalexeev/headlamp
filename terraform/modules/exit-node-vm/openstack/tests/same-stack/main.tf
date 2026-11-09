# Test fixture (#224, the #222 case): a root stack that creates the Barbican secret in the SAME apply as the node.
# The secret href is then unknown at plan time, so the module must not decide a count from it.
terraform {
  required_providers {
    openstack = { source = "terraform-provider-openstack/openstack" }
  }
}

resource "openstack_keymanager_secret_v1" "authkey" {
  name                 = "headlamp-node-b-authkey"
  payload              = "tskey-auth-0000"
  payload_content_type = "text/plain"
}

module "node" {
  source                 = "../.."
  name                   = "node-b"
  availability_zone      = "zone-a"
  flavor_id              = "1011"
  volume_type            = "fast.zone-a"
  ssh_public_key         = "ssh-ed25519 AAAAexample example"
  bootstrap_tailscale    = true
  headscale_login_server = "https://hs.example.com"
  authkey_from_secret    = true
  authkey_secret_ref     = openstack_keymanager_secret_v1.authkey.secret_ref
  keystone_auth_url      = "https://keystone.example.com/v3"
}

output "instance_id" {
  value = module.node.instance_id
}

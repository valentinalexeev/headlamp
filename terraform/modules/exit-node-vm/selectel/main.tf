# Compatibility wrapper: the module moved to ../openstack (#101). Existing root stacks keep working: every input and
# output is passed through unchanged and the defaults are the Selectel ones. New stacks use ../openstack directly.
module "node" {
  source                 = "../openstack"
  name                   = var.name
  tailscale_hostname     = var.tailscale_hostname
  availability_zone      = var.availability_zone
  flavor_id              = var.flavor_id
  volume_type            = var.volume_type
  disk_size_gb           = var.disk_size_gb
  image_name             = var.image_name
  subnet_cidr            = var.subnet_cidr
  ssh_public_key         = var.ssh_public_key
  admin_ssh_cidr         = var.admin_ssh_cidr
  tailscale_port         = var.tailscale_port
  preemptible            = var.preemptible
  bootstrap_tailscale    = var.bootstrap_tailscale
  headscale_login_server = var.headscale_login_server
  tailscale_authkey      = var.tailscale_authkey
  external_network_name  = var.external_network_name
}

# The resources used to live directly in this module. These blocks move existing state into module.node, so
# upgrading a root stack that already uses this module plans no change.
moved {
  from = openstack_compute_keypair_v2.this
  to   = module.node.openstack_compute_keypair_v2.this
}

moved {
  from = openstack_networking_network_v2.this
  to   = module.node.openstack_networking_network_v2.this
}

moved {
  from = openstack_networking_subnet_v2.this
  to   = module.node.openstack_networking_subnet_v2.this
}

moved {
  from = openstack_networking_router_v2.this
  to   = module.node.openstack_networking_router_v2.this
}

moved {
  from = openstack_networking_router_interface_v2.this
  to   = module.node.openstack_networking_router_interface_v2.this
}

moved {
  from = openstack_networking_secgroup_v2.this
  to   = module.node.openstack_networking_secgroup_v2.this
}

moved {
  from = openstack_networking_secgroup_rule_v2.tailscale_in
  to   = module.node.openstack_networking_secgroup_rule_v2.tailscale_in
}

moved {
  from = openstack_networking_secgroup_rule_v2.ssh_in
  to   = module.node.openstack_networking_secgroup_rule_v2.ssh_in
}

moved {
  from = openstack_networking_port_v2.this
  to   = module.node.openstack_networking_port_v2.this
}

moved {
  from = openstack_blockstorage_volume_v3.boot
  to   = module.node.openstack_blockstorage_volume_v3.boot
}

moved {
  from = openstack_compute_instance_v2.this
  to   = module.node.openstack_compute_instance_v2.this
}

moved {
  from = openstack_networking_floatingip_v2.this
  to   = module.node.openstack_networking_floatingip_v2.this
}

moved {
  from = openstack_networking_floatingip_associate_v2.this
  to   = module.node.openstack_networking_floatingip_associate_v2.this
}


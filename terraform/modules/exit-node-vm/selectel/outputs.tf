output "instance_id" {
  value = openstack_compute_instance_v2.this.id
}

output "external_ip" {
  value = openstack_networking_floatingip_v2.this.address
}

output "security_group_id" {
  description = "ID of the node's security group, for attaching extra rules from the root stack."
  value       = openstack_networking_secgroup_v2.this.id
}

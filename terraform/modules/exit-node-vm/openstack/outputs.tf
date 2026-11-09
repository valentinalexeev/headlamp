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

output "image_id" {
  description = "The image of the boot volume (image_id, or the newest image named image_name at the last plan)."
  value       = local.image_id
}

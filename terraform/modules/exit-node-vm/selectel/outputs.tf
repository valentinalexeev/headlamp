output "instance_id" {
  value = module.node.instance_id
}

output "external_ip" {
  value = module.node.external_ip
}

output "security_group_id" {
  description = "ID of the node's security group, for attaching extra rules from the root stack."
  value       = module.node.security_group_id
}

output "image_id" {
  value = module.node.image_id
}

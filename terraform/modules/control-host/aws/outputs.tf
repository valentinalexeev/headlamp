output "public_address" {
  description = "Point the DNS names of headscale, the enrolment listener and the funnel wildcards here: a CNAME to the load balancer, or an A record to the Elastic IP."
  value       = var.load_balancer ? aws_lb.this[0].dns_name : aws_eip.this[0].public_ip
}

output "load_balancer_zone_id" {
  description = "Hosted zone id of the load balancer, for a Route 53 alias record. Null without a load balancer."
  value       = var.load_balancer ? aws_lb.this[0].zone_id : null
}

output "instance_id" {
  description = "For SSM Session Manager: aws ssm start-session --target <id>."
  value       = aws_instance.this.id
}

output "data_volume_id" {
  value = aws_ebs_volume.data.id
}

output "security_group_id" {
  value = aws_security_group.this.id
}

output "role_name" {
  description = "Instance role. Attach more policies to it if the host needs them."
  value       = aws_iam_role.this.name
}

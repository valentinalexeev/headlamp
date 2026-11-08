output "security_group_id" {
  value = aws_security_group.this.id
}

output "autoscaling_group_name" {
  value = aws_autoscaling_group.this.name
}

output "role_name" {
  value       = try(aws_iam_role.this[0].name, null)
  description = "The node role, when authkey_secret_arn or enable_ssm is set. Null otherwise."
}

output "launch_template_id" {
  value = aws_launch_template.this.id
}

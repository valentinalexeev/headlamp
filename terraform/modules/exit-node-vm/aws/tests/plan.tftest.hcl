# Plan-only checks with a mocked AWS provider: no account and no credentials needed.
#   cd terraform/modules/exit-node-vm/aws && terraform init -backend=false && terraform test
mock_provider "aws" {
  mock_data "aws_vpc" {
    defaults = { id = "vpc-00000000" }
  }
  mock_data "aws_subnets" {
    defaults = { ids = ["subnet-00000001", "subnet-00000002"] }
  }
  mock_data "aws_ami" {
    defaults = { id = "ami-00000000" }
  }
  mock_data "aws_region" {
    defaults = { region = "eu-central-1" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
  mock_resource "aws_iam_instance_profile" {
    defaults = { arn = "arn:aws:iam::123456789012:instance-profile/n1-exit-node" }
  }
}

variables {
  name                   = "n1"
  headscale_login_server = "https://hs.example.com"
}

run "legacy_caller_keeps_the_old_user_data" {
  command = plan
  variables {
    tailscale_authkey = "tskey-legacy-0000"
    ssh_public_key    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE0000000000000000000000000000000000000000000 test"
  }

  assert {
    condition = base64decode(aws_launch_template.this.user_data) == templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
      headscale_login_server = "https://hs.example.com", tailscale_authkey = "tskey-legacy-0000", hostname = "n1"
    })
    error_message = "a caller with tailscale_authkey must keep the v0.12.0 user data"
  }
  assert {
    condition     = length(aws_key_pair.this) == 1 && aws_launch_template.this.key_name == "n1-admin"
    error_message = "ssh_public_key must still make the key pair"
  }
  assert {
    condition     = length(aws_iam_role.this) == 0 && length(aws_launch_template.this.iam_instance_profile) == 0
    error_message = "no role without authkey_secret_arn or enable_ssm"
  }
  assert {
    condition     = aws_launch_template.this.metadata_options[0].http_tokens == "required" && aws_launch_template.this.metadata_options[0].http_put_response_hop_limit == 1
    error_message = "IMDSv2 must be required with hop limit 1"
  }
}

run "secret_keeps_the_key_out_of_user_data" {
  command = plan
  variables {
    authkey_secret_arn = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:headlamp/n1-authkey-AbCdEf"
  }

  assert {
    condition     = !strcontains(base64decode(aws_launch_template.this.user_data), "tskey")
    error_message = "the user data must contain no auth key"
  }
  assert {
    condition     = strcontains(base64decode(aws_launch_template.this.user_data), "--auth-key=file:/run/headlamp/authkey")
    error_message = "tailscale up must read the key from a file"
  }
  assert {
    condition     = length(aws_iam_role.this) == 1 && length(aws_iam_role_policy.authkey) == 1 && length(aws_iam_role_policy_attachment.ssm) == 0
    error_message = "the secret needs a role with the read policy, and no SSM by default"
  }
  assert {
    condition     = length(aws_launch_template.this.iam_instance_profile) == 1 && length(aws_iam_instance_profile.this) == 1
    error_message = "the launch template must use the instance profile"
  }
  assert {
    condition     = length(aws_key_pair.this) == 0 && aws_launch_template.this.key_name == null
    error_message = "no key pair without ssh_public_key"
  }
}

run "rejects_both_key_sources" {
  command = plan
  variables {
    tailscale_authkey  = "tskey-0000"
    authkey_secret_arn = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:x-AbCdEf"
  }
  expect_failures = [aws_launch_template.this]
}

run "rejects_no_key_source" {
  command         = plan
  expect_failures = [aws_launch_template.this]
}

run "rejects_a_malformed_secret_arn" {
  command = plan
  variables {
    authkey_secret_arn = "headlamp/n1-authkey"
  }
  expect_failures = [var.authkey_secret_arn]
}

run "ssm_iam_path_and_boundary" {
  command = plan
  variables {
    tailscale_authkey        = "tskey-0000"
    enable_ssm               = true
    iam_path                 = "/headlamp/"
    iam_permissions_boundary = "arn:aws:iam::123456789012:policy/boundary"
  }

  assert {
    condition     = length(aws_iam_role_policy_attachment.ssm) == 1 && length(aws_iam_role_policy.authkey) == 0
    error_message = "enable_ssm must attach SSM and no secret policy"
  }
  assert {
    condition     = aws_iam_role.this[0].path == "/headlamp/" && aws_iam_role.this[0].permissions_boundary == "arn:aws:iam::123456789012:policy/boundary" && aws_iam_instance_profile.this[0].path == "/headlamp/"
    error_message = "iam_path and iam_permissions_boundary must reach the role and the profile"
  }
}

run "tags_reach_every_resource_and_the_group" {
  command = plan
  variables {
    tailscale_authkey = "tskey-0000"
    tags              = { team = "net", Name = "ignored-in-asg" }
  }

  assert {
    condition     = aws_security_group.this.tags["team"] == "net" && aws_launch_template.this.tags["team"] == "net"
    error_message = "tags must reach the security group and the launch template"
  }
  assert {
    condition     = contains([for t in aws_autoscaling_group.this.tag : "${t.key}=${t.value}=${t.propagate_at_launch}"], "team=net=true")
    error_message = "the group must propagate the tags at launch"
  }
  assert {
    condition     = length([for t in aws_autoscaling_group.this.tag : t if t.key == "Name"]) == 1
    error_message = "a Name in tags must not make a second Name tag on the group"
  }
  assert {
    condition     = contains([for s in aws_launch_template.this.tag_specifications : s.resource_type], "volume")
    error_message = "tags must reach the volumes"
  }
}

run "pinned_tailscale_from_the_signed_repo" {
  command = plan
  variables {
    tailscale_authkey = "tskey-0000"
    tailscale_version = "1.104.5"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.this.user_data), "tailscale=1.104.5") && !strcontains(base64decode(aws_launch_template.this.user_data), "install.sh")
    error_message = "tailscale_version must install that version from the apt repo, not install.sh"
  }
}

run "own_vpc_and_subnets" {
  command = plan
  variables {
    tailscale_authkey = "tskey-0000"
    vpc_id            = "vpc-0123456789abcdef0"
    subnet_ids        = ["subnet-0aaa", "subnet-0bbb"]
  }

  assert {
    condition     = length(data.aws_vpc.default) == 0 && length(data.aws_subnets.default) == 0
    error_message = "with vpc_id and subnet_ids there must be no default VPC lookup"
  }
  assert {
    condition     = aws_security_group.this.vpc_id == "vpc-0123456789abcdef0" && toset(aws_autoscaling_group.this.vpc_zone_identifier) == toset(["subnet-0aaa", "subnet-0bbb"])
    error_message = "the security group and the group must use the given VPC and subnets"
  }
}

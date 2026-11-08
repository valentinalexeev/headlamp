# Plan-only checks with a mocked AWS provider: no account and no credentials needed.
#   cd terraform/modules/control-host/aws && terraform init -backend=false && terraform test
mock_provider "aws" {
  mock_data "aws_vpc" {
    defaults = { id = "vpc-00000000" }
  }
  mock_data "aws_subnets" {
    defaults = { ids = ["subnet-00000001", "subnet-00000002"] }
  }
  mock_data "aws_subnet" {
    defaults = { id = "subnet-00000001", availability_zone = "zz-test-1a" }
  }
  mock_data "aws_ami" {
    defaults = { id = "ami-00000000" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
}

variables {
  name = "headlamp-control"
}

run "defaults_use_a_load_balancer" {
  command = plan

  assert {
    condition     = length(aws_lb.this) == 1 && aws_lb.this[0].load_balancer_type == "network"
    error_message = "the default must be one network load balancer"
  }
  assert {
    condition     = length(aws_eip.this) == 0
    error_message = "no Elastic IP with a load balancer"
  }
  assert {
    condition     = toset(keys(aws_lb_listener.this)) == toset(["tcp443", "tcp80", "udp3478"])
    error_message = "listeners must be 443/tcp, 80/tcp and 3478/udp"
  }
  assert {
    condition     = alltrue([for tg in aws_lb_target_group.this : tg.preserve_client_ip == "true" && tg.target_type == "instance"])
    error_message = "targets must keep the client address"
  }
  assert {
    condition     = aws_ebs_volume.data.encrypted && aws_ebs_volume.data.availability_zone == "zz-test-1a"
    error_message = "the data volume must be encrypted and in the host's zone"
  }
  assert {
    condition     = aws_instance.this.metadata_options[0].http_tokens == "required"
    error_message = "IMDSv2 must be required"
  }
  assert {
    condition     = length(aws_dlm_lifecycle_policy.data) == 1
    error_message = "snapshots must be on by default"
  }
  assert {
    condition     = length(aws_iam_role_policy.dns01) == 0 && length(aws_key_pair.this) == 0
    error_message = "no DNS rights and no SSH key unless asked for"
  }
  assert {
    condition     = length([for r in aws_security_group.this.ingress : r if r.from_port == 22]) == 0
    error_message = "no SSH rule by default"
  }
}

run "elastic_ip_without_load_balancer" {
  command = plan
  variables {
    load_balancer        = false
    snapshot_retain_days = 0
    admin_cidrs          = ["192.0.2.0/24"]
    route53_zone_id      = "Z0000000000000"
    ssh_public_key       = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyOnlyForTests test"
  }

  assert {
    condition     = length(aws_lb.this) == 0 && length(aws_lb_listener.this) == 0 && length(aws_eip.this) == 1
    error_message = "without a load balancer the host gets an Elastic IP"
  }
  assert {
    condition     = length(aws_dlm_lifecycle_policy.data) == 0
    error_message = "snapshot_retain_days = 0 turns snapshots off"
  }
  assert {
    condition     = length([for r in aws_security_group.this.ingress : r if r.from_port == 22 && contains(r.cidr_blocks, "192.0.2.0/24")]) == 1
    error_message = "admin_cidrs opens SSH to those addresses only"
  }
  assert {
    condition     = length(aws_iam_role_policy.dns01) == 1 && length(aws_key_pair.this) == 1
    error_message = "route53_zone_id and ssh_public_key are honoured"
  }
}

run "rejects_unknown_arch" {
  command = plan
  variables {
    ami_arch = "riscv64"
  }
  expect_failures = [var.ami_arch]
}

run "iam_defaults_keep_the_root_path_and_no_boundary" {
  command = plan

  assert {
    condition     = aws_iam_role.this.path == "/" && aws_iam_instance_profile.this.path == "/" && aws_iam_role.dlm[0].path == "/"
    error_message = "the default IAM path must stay /"
  }
  assert {
    condition     = aws_iam_role.this.permissions_boundary == null && aws_iam_role.dlm[0].permissions_boundary == null
    error_message = "no permissions boundary unless asked for"
  }
}

run "iam_path_and_permissions_boundary" {
  command = plan
  variables {
    iam_path                 = "/headlamp/"
    iam_permissions_boundary = "arn:aws:iam::123456789012:policy/example-boundary"
  }

  assert {
    condition     = aws_iam_role.this.path == "/headlamp/" && aws_iam_role.dlm[0].path == "/headlamp/" && aws_iam_instance_profile.this.path == "/headlamp/"
    error_message = "both roles and the instance profile must use iam_path"
  }
  assert {
    condition = alltrue([for b in [aws_iam_role.this.permissions_boundary, aws_iam_role.dlm[0].permissions_boundary] :
    b == "arn:aws:iam::123456789012:policy/example-boundary"])
    error_message = "both roles must carry the permissions boundary"
  }
}

run "rejects_a_malformed_iam_path" {
  command = plan
  variables {
    iam_path = "headlamp"
  }
  expect_failures = [var.iam_path]
}

run "rejects_a_boundary_that_is_not_a_policy_arn" {
  command = plan
  variables {
    iam_permissions_boundary = "example-boundary"
  }
  expect_failures = [var.iam_permissions_boundary]
}

run "image_defaults_to_ubuntu_minimal" {
  command = plan

  assert {
    condition     = length(data.aws_ami.ubuntu) == 1 && local.ami_name_pattern == "ubuntu-minimal/images/hvm-ssd-gp3/ubuntu-resolute-26.04-amd64-minimal-*"
    error_message = "the default image must be the Canonical Ubuntu 26.04 Minimal AMI"
  }
  assert {
    condition     = aws_instance.this.ami == "ami-00000000"
    error_message = "the instance must use the looked-up AMI"
  }
}

run "image_flavor_server" {
  command = plan
  variables {
    image_flavor = "server"
    ami_arch     = "arm64"
  }

  assert {
    condition     = local.ami_name_pattern == "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-arm64-server-*"
    error_message = "image_flavor = server must use the Ubuntu Server AMI"
  }
}

run "ami_id_skips_the_lookup" {
  command = plan
  variables {
    ami_id = "ami-0123456789abcdef0"
  }

  assert {
    condition     = length(data.aws_ami.ubuntu) == 0 && aws_instance.this.ami == "ami-0123456789abcdef0"
    error_message = "ami_id must set the AMI and skip the lookup"
  }
}

run "rejects_unknown_image_flavor" {
  command = plan
  variables {
    image_flavor = "desktop"
  }
  expect_failures = [var.image_flavor]
}

run "imds_hop_limit_defaults_to_1" {
  command = plan

  assert {
    condition     = aws_instance.this.metadata_options[0].http_put_response_hop_limit == 1 && aws_instance.this.metadata_options[0].http_tokens == "required"
    error_message = "the default must be IMDSv2 only with hop limit 1"
  }
}

run "imds_hop_limit_2_for_bridge_containers" {
  command = plan
  variables {
    metadata_hop_limit = 2
  }

  assert {
    condition     = aws_instance.this.metadata_options[0].http_put_response_hop_limit == 2 && aws_instance.this.metadata_options[0].http_tokens == "required"
    error_message = "metadata_hop_limit must set the hop limit and keep IMDSv2 required"
  }
}

run "rejects_a_hop_limit_out_of_range" {
  command = plan
  variables {
    metadata_hop_limit = 0
  }
  expect_failures = [var.metadata_hop_limit]
}

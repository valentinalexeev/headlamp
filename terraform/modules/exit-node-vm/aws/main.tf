data "aws_vpc" "default" {
  count   = var.vpc_id == null ? 1 : 0
  default = true
}

data "aws_subnets" "default" {
  count = var.subnet_ids == null ? 1 : 0
  filter {
    name   = "vpc-id"
    values = [local.vpc_id]
  }
}

locals {
  # Canonical publishes Minimal AMIs under a separate name prefix and product suffix.
  ami_name_pattern = (var.image_flavor == "minimal"
    ? "ubuntu-minimal/images/hvm-ssd-gp3/ubuntu-${var.ubuntu_series}-${var.ami_arch}-minimal-*"
  : "ubuntu/images/hvm-ssd-gp3/ubuntu-${var.ubuntu_series}-${var.ami_arch}-server-*")

  control_tag = "headlamp-exit-node"
  vpc_id      = coalesce(var.vpc_id, try(data.aws_vpc.default[0].id, null))
  subnet_ids  = var.subnet_ids != null ? var.subnet_ids : data.aws_subnets.default[0].ids
  hostname    = coalesce(var.tailscale_hostname, var.name)

  # The new template is used only when a new feature asks for it, so the user data of existing callers does not
  # change (a change makes a new launch template version).
  use_v2_template = var.authkey_secret_arn != null || var.tailscale_version != null || var.state_parameter_name != null

  # tailscaled's SSM state store (#213): --state=arn:aws:ssm:<region>:<account>:parameter<name>[?kmsKey=<arn>].
  state_param_arn = var.state_parameter_name == null ? null : "arn:${data.aws_partition.current[0].partition}:ssm:${data.aws_region.current.region}:${data.aws_caller_identity.current[0].account_id}:parameter${var.state_parameter_name}"
  state_arn       = local.state_param_arn == null ? null : (var.state_kms_key_arn == null ? local.state_param_arn : "${local.state_param_arn}?kmsKey=${var.state_kms_key_arn}")

  user_data = local.use_v2_template ? templatefile("${path.module}/templates/cloud-init-v2.yaml.tmpl", {
    headscale_login_server = var.headscale_login_server
    tailscale_authkey      = var.tailscale_authkey
    authkey_secret_arn     = var.authkey_secret_arn
    tailscale_version      = var.tailscale_version
    state_arn              = local.state_arn
    hostname               = local.hostname
    fetch_secret_b64       = filebase64("${path.module}/templates/fetch-secret.py")
    }) : templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    headscale_login_server = var.headscale_login_server
    tailscale_authkey      = var.tailscale_authkey == null ? "" : var.tailscale_authkey # "" only when the precondition fails
    hostname               = local.hostname
  })

  # The node needs a role only to read the secret or for SSM.
  create_role = var.authkey_secret_arn != null || var.enable_ssm || var.state_parameter_name != null
}


data "aws_ami" "ubuntu" {
  count       = var.ami_id == null ? 1 : 0
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = [local.ami_name_pattern]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "architecture"
    values = [var.ami_arch == "arm64" ? "arm64" : "x86_64"]
  }
}

resource "aws_security_group" "this" {
  name = "${var.name}-exit-node"
  # No public SSH ingress, deliberately -- matches the other VM modules: admin
  # access happens over the tailnet once the node joins (ssh to its
  # <name>.<your-tailnet-domain>, tunneled inside the WireGuard UDP flow below,
  # which the AWS security group never inspects at the TCP:22 level), not
  # via a public-internet rule pinned to an admin IP that will drift.
  description = "Tailscale exit node: WireGuard UDP only, no public SSH"
  vpc_id      = local.vpc_id

  ingress {
    description = "Tailscale WireGuard"
    from_port   = 41641
    to_port     = 41641
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.name}-exit-node"
  })
}

resource "aws_key_pair" "this" {
  count      = var.ssh_public_key == null ? 0 : 1
  key_name   = "${var.name}-admin"
  public_key = var.ssh_public_key
  tags       = var.tags
}

# --- node role: read the auth key secret, optional SSM

data "aws_iam_policy_document" "assume" {
  count = local.create_role ? 1 : 0
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "this" {
  count                = local.create_role ? 1 : 0
  name                 = "${var.name}-exit-node"
  path                 = var.iam_path
  permissions_boundary = var.iam_permissions_boundary
  assume_role_policy   = data.aws_iam_policy_document.assume[0].json
  tags                 = var.tags
}

data "aws_iam_policy_document" "authkey" {
  count = var.authkey_secret_arn == null ? 0 : 1
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [var.authkey_secret_arn]
  }
  dynamic "statement" {
    for_each = var.authkey_kms_key_arn == null ? [] : [var.authkey_kms_key_arn]
    content {
      actions   = ["kms:Decrypt"]
      resources = [statement.value]
      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["secretsmanager.${data.aws_region.current.region}.amazonaws.com"]
      }
    }
  }
}

data "aws_region" "current" {}

data "aws_caller_identity" "current" {
  count = var.state_parameter_name == null ? 0 : 1
}

data "aws_partition" "current" {
  count = var.state_parameter_name == null ? 0 : 1
}

# tailscaled reads the whole state once at start and writes it back on each change (#213).
data "aws_iam_policy_document" "state" {
  count = var.state_parameter_name == null ? 0 : 1
  statement {
    actions   = ["ssm:GetParameter", "ssm:PutParameter"]
    resources = [local.state_param_arn]
  }
  dynamic "statement" {
    for_each = var.state_kms_key_arn == null ? [] : [var.state_kms_key_arn]
    content {
      actions   = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"]
      resources = [statement.value]
      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["ssm.${data.aws_region.current.region}.amazonaws.com"]
      }
    }
  }
}

resource "aws_iam_role_policy" "state" {
  count  = var.state_parameter_name == null ? 0 : 1
  name   = "tailscaled-state"
  role   = aws_iam_role.this[0].id
  policy = data.aws_iam_policy_document.state[0].json
}

resource "aws_iam_role_policy" "authkey" {
  count  = var.authkey_secret_arn == null ? 0 : 1
  name   = "read-authkey"
  role   = aws_iam_role.this[0].id
  policy = data.aws_iam_policy_document.authkey[0].json
}

resource "aws_iam_role_policy_attachment" "ssm" {
  count      = var.enable_ssm ? 1 : 0
  role       = aws_iam_role.this[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "this" {
  count = local.create_role ? 1 : 0
  name  = "${var.name}-exit-node"
  path  = var.iam_path
  role  = aws_iam_role.this[0].name
  tags  = var.tags
}

resource "aws_launch_template" "this" {
  name_prefix   = "${var.name}-"
  image_id      = coalesce(var.ami_id, try(data.aws_ami.ubuntu[0].id, null))
  instance_type = var.instance_type
  key_name      = try(aws_key_pair.this[0].key_name, null)

  vpc_security_group_ids = [aws_security_group.this.id]

  dynamic "iam_instance_profile" {
    for_each = local.create_role ? [1] : []
    content {
      arn = aws_iam_instance_profile.this[0].arn
    }
  }

  # IMDSv2 only. Hop limit 1: only the host gets the role.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  dynamic "instance_market_options" {
    for_each = var.spot ? [1] : []
    content {
      market_type = "spot"
      spot_options {
        spot_instance_type = "one-time"
      }
    }
  }

  user_data = base64encode(local.user_data)

  tag_specifications {
    resource_type = "instance"
    tags = merge(var.tags, {
      Name                = var.name
      (local.control_tag) = "true"
    })
  }

  dynamic "tag_specifications" {
    for_each = length(var.tags) > 0 ? [1] : []
    content {
      resource_type = "volume"
      tags          = merge(var.tags, { Name = var.name })
    }
  }

  lifecycle {
    precondition {
      condition     = (var.authkey_secret_arn == null) != (var.tailscale_authkey == null)
      error_message = "Set exactly one of authkey_secret_arn and tailscale_authkey."
    }
  }

  # Tags the launch template resource itself (not just the instances it
  # spawns, above) -- lets the headlamp-terraform IAM policy scope
  # Modify/Delete on it by aws:ResourceTag post-creation.
  tags = merge(var.tags, {
    Name = var.name
  })
}

resource "aws_autoscaling_group" "this" {
  name                = "${var.name}-asg"
  desired_capacity    = 1
  min_size            = 1
  max_size            = 1
  vpc_zone_identifier = local.subnet_ids
  health_check_type   = "EC2"

  launch_template {
    id      = aws_launch_template.this.id
    version = "$Latest"
  }

  tag {
    key                 = "Name"
    value               = var.name
    propagate_at_launch = true
  }

  # node-control's aws provider scales this group to stop and start the node; its IAM policy is limited to resources
  # with this tag (see apps/node-control/README.md, "AWS permissions").
  tag {
    key                 = local.control_tag
    value               = "true"
    propagate_at_launch = true
  }

  # Provider default_tags do not reach ASG tag blocks, so var.tags is set here too.
  dynamic "tag" {
    for_each = { for k, v in var.tags : k => v if !contains(["Name", local.control_tag], k) }
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }

  # node-control sets desired_capacity and min_size to 0 to stop the node and back to 1 to start it. Terraform must not
  # undo that on the next apply.
  lifecycle {
    ignore_changes = [desired_capacity, min_size]
  }
}

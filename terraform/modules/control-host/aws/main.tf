data "aws_vpc" "this" {
  id      = var.vpc_id
  default = var.vpc_id == null ? true : null
}

data "aws_subnets" "this" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.this.id]
  }
}

data "aws_subnet" "this" {
  id = coalesce(var.subnet_id, sort(data.aws_subnets.this.ids)[0])
}

locals {
  # Canonical publishes Minimal AMIs under a separate name prefix and product suffix.
  ami_name_pattern = (var.image_flavor == "minimal"
    ? "ubuntu-minimal/images/hvm-ssd-gp3/ubuntu-resolute-26.04-${var.ami_arch}-minimal-*"
  : "ubuntu/images/hvm-ssd-gp3/ubuntu-resolute-26.04-${var.ami_arch}-server-*")
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
    name   = "architecture"
    values = [var.ami_arch == "arm64" ? "arm64" : "x86_64"]
  }
}

locals {
  tags = merge(var.tags, { "headlamp:role" = "control-host" })
  # Public ports. 443/tcp: headscale (HTTPS, DERP), node-control enrolment and funnel-ingress, by name on the TLS
  # edge. 80/tcp: ACME HTTP-01 and the redirect to HTTPS. 3478/udp: STUN for the embedded DERP server.
  ports = [
    { port = 443, protocol = "tcp" },
    { port = 80, protocol = "tcp" },
    { port = 3478, protocol = "udp" },
  ]
}

resource "aws_security_group" "this" {
  name        = "${var.name}-host"
  description = "headlamp control host: 443/tcp, 80/tcp, 3478/udp"
  vpc_id      = data.aws_vpc.this.id

  # The load balancer keeps the client address (instance targets), so the rules are the same with and without it.
  dynamic "ingress" {
    for_each = local.ports
    content {
      description = "public ${ingress.value.port}/${ingress.value.protocol}"
      from_port   = ingress.value.port
      to_port     = ingress.value.port
      protocol    = ingress.value.protocol
      cidr_blocks = ["0.0.0.0/0"]
    }
  }

  dynamic "ingress" {
    for_each = length(var.admin_cidrs) > 0 ? [1] : []
    content {
      description = "SSH from admin addresses"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = var.admin_cidrs
    }
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, { Name = "${var.name}-host" })
}

# --- instance role: SSM Session Manager, and optional DNS-01 on one hosted zone

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "this" {
  name                 = "${var.name}-host"
  path                 = var.iam_path
  permissions_boundary = var.iam_permissions_boundary
  assume_role_policy   = data.aws_iam_policy_document.assume.json
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.this.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "dns01" {
  count = var.route53_zone_id == null ? 0 : 1
  statement {
    actions   = ["route53:ChangeResourceRecordSets", "route53:ListResourceRecordSets"]
    resources = ["arn:aws:route53:::hostedzone/${var.route53_zone_id}"]
  }
  statement {
    actions   = ["route53:GetChange", "route53:ListHostedZones", "route53:ListHostedZonesByName"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "dns01" {
  count  = var.route53_zone_id == null ? 0 : 1
  name   = "acme-dns01"
  role   = aws_iam_role.this.id
  policy = data.aws_iam_policy_document.dns01[0].json
}

resource "aws_iam_instance_profile" "this" {
  name = "${var.name}-host"
  path = var.iam_path
  role = aws_iam_role.this.name
}

# --- the host and its data volume

resource "aws_key_pair" "this" {
  count      = var.ssh_public_key == null ? 0 : 1
  key_name   = "${var.name}-admin"
  public_key = var.ssh_public_key
}

resource "aws_ebs_volume" "data" {
  availability_zone = data.aws_subnet.this.availability_zone
  size              = var.data_volume_gb
  type              = "gp3"
  encrypted         = true
  snapshot_id       = var.data_snapshot_id
  tags              = merge(local.tags, { Name = "${var.name}-data", "headlamp:snapshot" = var.name })

  lifecycle {
    # The volume holds the tailnet: replacing the instance must never replace it.
    prevent_destroy = true
    # data_snapshot_id is for a restore into a new volume; changing it later must not replace this one.
    ignore_changes = [snapshot_id]
  }
}

resource "aws_instance" "this" {
  ami                    = coalesce(var.ami_id, try(data.aws_ami.ubuntu[0].id, null))
  instance_type          = var.instance_type
  subnet_id              = data.aws_subnet.this.id
  vpc_security_group_ids = [aws_security_group.this.id]
  iam_instance_profile   = aws_iam_instance_profile.this.name
  key_name               = var.ssh_public_key == null ? null : aws_key_pair.this[0].key_name

  # A public address for egress (image pulls, ACME). With load_balancer = false the Elastic IP replaces it.
  associate_public_ip_address = true

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = var.metadata_hop_limit
  }

  root_block_device {
    volume_size = var.root_volume_gb
    volume_type = "gp3"
    encrypted   = true
    # Tagged in the RunInstances request, so a deployer that may create volumes only with the site tags can launch
    # the host (#225). Not volume_tags: that would also set the tags of the separate data volume.
    tags = merge(local.tags, { Name = "${var.name}-root" })
  }

  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    volume_id       = aws_ebs_volume.data.id
    user_data_extra = var.user_data_extra
  })
  user_data_replace_on_change = true

  tags = merge(local.tags, { Name = var.name })

  lifecycle {
    # A new AMI (a newer image, image_flavor or ami_id) does not replace a running host.
    ignore_changes = [ami]
  }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.this.id
}

# The primary network interface. aws_instance cannot tag it in the RunInstances request, so it is tagged right after
# the launch (#225), for cost reports and tag-based cleanup. A deployer whose policy needs the site tags on
# network-interface/* in RunInstances must allow it without the tag condition (README "Deploying identity").
resource "aws_ec2_tag" "eni" {
  for_each    = merge(local.tags, { Name = "${var.name}-eni" })
  resource_id = aws_instance.this.primary_network_interface_id
  key         = each.key
  value       = each.value
}

resource "aws_eip" "this" {
  count    = var.load_balancer ? 0 : 1
  domain   = "vpc"
  instance = aws_instance.this.id
  tags     = merge(local.tags, { Name = var.name })
}

# --- network load balancer: TCP passes through, TLS ends on the host

resource "aws_lb" "this" {
  count              = var.load_balancer ? 1 : 0
  name               = substr(var.name, 0, 32)
  load_balancer_type = "network"
  internal           = false
  subnets            = [data.aws_subnet.this.id]
  tags               = local.tags
}

resource "aws_lb_target_group" "this" {
  for_each = var.load_balancer ? { for p in local.ports : "${p.protocol}${p.port}" => p } : {}

  name               = substr("${var.name}-${each.key}", 0, 32)
  port               = each.value.port
  protocol           = upper(each.value.protocol)
  vpc_id             = data.aws_vpc.this.id
  target_type        = "instance"
  preserve_client_ip = true

  # UDP targets cannot be checked over UDP; every target group checks the HTTPS port over TCP.
  health_check {
    protocol = "TCP"
    port     = "443"
  }

  tags = local.tags
}

resource "aws_lb_target_group_attachment" "this" {
  for_each         = aws_lb_target_group.this
  target_group_arn = each.value.arn
  target_id        = aws_instance.this.id
}

resource "aws_lb_listener" "this" {
  for_each          = aws_lb_target_group.this
  load_balancer_arn = aws_lb.this[0].arn
  port              = each.value.port
  protocol          = each.value.protocol

  default_action {
    type             = "forward"
    target_group_arn = each.value.arn
  }
}

# --- daily snapshots of the data volume

data "aws_iam_policy_document" "dlm_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["dlm.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "dlm" {
  count                = var.snapshot_retain_days > 0 ? 1 : 0
  name                 = "${var.name}-dlm"
  path                 = var.iam_path
  permissions_boundary = var.iam_permissions_boundary
  assume_role_policy   = data.aws_iam_policy_document.dlm_assume.json
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "dlm" {
  count      = var.snapshot_retain_days > 0 ? 1 : 0
  role       = aws_iam_role.dlm[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole"
}

resource "aws_dlm_lifecycle_policy" "data" {
  count              = var.snapshot_retain_days > 0 ? 1 : 0
  description        = "${var.name} daily snapshots of the data volume" # no colon: DLM allows [0-9A-Za-z _-] only
  execution_role_arn = aws_iam_role.dlm[0].arn
  state              = "ENABLED"

  policy_details {
    resource_types = ["VOLUME"]
    target_tags    = { "headlamp:snapshot" = var.name }

    schedule {
      name      = "daily"
      copy_tags = true
      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["03:00"]
      }
      retain_rule {
        count = var.snapshot_retain_days
      }
    }
  }

  tags = local.tags
}

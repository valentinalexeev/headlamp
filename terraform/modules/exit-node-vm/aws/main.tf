data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}

data "aws_ami" "ubuntu_noble" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-${var.ami_arch}-server-*"]
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
  vpc_id      = data.aws_vpc.default.id

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

  tags = {
    Name = "${var.name}-exit-node"
  }
}

resource "aws_key_pair" "this" {
  key_name   = "${var.name}-admin"
  public_key = var.ssh_public_key
}

resource "aws_launch_template" "this" {
  name_prefix   = "${var.name}-"
  image_id      = data.aws_ami.ubuntu_noble.id
  instance_type = var.instance_type
  key_name      = aws_key_pair.this.key_name

  vpc_security_group_ids = [aws_security_group.this.id]

  dynamic "instance_market_options" {
    for_each = var.spot ? [1] : []
    content {
      market_type = "spot"
      spot_options {
        spot_instance_type = "one-time"
      }
    }
  }

  user_data = base64encode(templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    headscale_login_server = var.headscale_login_server
    tailscale_authkey      = var.tailscale_authkey
    hostname               = coalesce(var.tailscale_hostname, var.name)
  }))

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name = var.name
    }
  }

  # Tags the launch template resource itself (not just the instances it
  # spawns, above) -- lets the headlamp-terraform IAM policy scope
  # Modify/Delete on it by aws:ResourceTag post-creation.
  tags = {
    Name = var.name
  }
}

resource "aws_autoscaling_group" "this" {
  name                = "${var.name}-asg"
  desired_capacity    = 1
  min_size            = 1
  max_size            = 1
  vpc_zone_identifier = data.aws_subnets.default.ids
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
}

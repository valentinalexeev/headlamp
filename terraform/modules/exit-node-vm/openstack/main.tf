locals {
  hostname = coalesce(var.tailscale_hostname, var.name)
  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    bootstrap_tailscale    = var.bootstrap_tailscale
    headscale_login_server = var.headscale_login_server
    tailscale_authkey      = var.authkey_from_secret ? "" : var.tailscale_authkey
    hostname               = local.hostname
    advertise_exit_node    = var.advertise_exit_node
    use_secret             = var.authkey_from_secret
    fetch_secret_b64       = filebase64("${path.module}/templates/fetch-secret.py")
    credential_b64 = var.authkey_from_secret ? base64encode(jsonencode({
      auth_url    = var.keystone_auth_url
      id          = openstack_identity_application_credential_v3.authkey[0].id
      secret      = openstack_identity_application_credential_v3.authkey[0].secret
      secret_href = var.authkey_secret_ref
    })) : ""
  })

  # The access rule path is relative to the key-manager endpoint: /v1/secrets/<uuid>/payload.
  authkey_secret_id = var.authkey_secret_ref == null ? null : regex("[0-9a-fA-F-]{36}$", var.authkey_secret_ref)

  # An image ID pins the image (#235). A name picks the newest public image with that name at each plan.
  image_id = var.image_id != null ? var.image_id : data.openstack_images_image_v2.this[0].id

  # null (not []) when empty, so that a stack without tags plans no change (#227).
  tags          = length(var.tags) > 0 ? var.tags : null
  instance_tags = var.preemptible || length(var.tags) > 0 ? distinct(concat(var.tags, var.preemptible ? ["preemptible"] : [])) : null
  metadata      = length(var.metadata) > 0 ? var.metadata : null
}

data "openstack_images_image_v2" "this" {
  count       = var.image_id == null ? 1 : 0
  name        = var.image_name
  most_recent = true
  visibility  = "public"
}

data "openstack_networking_network_v2" "external" {
  external = true
  name     = var.external_network_name
}

# The node's only credential (#224): it can GET one Barbican secret payload and nothing else. It is in user data, so
# any process on the node can read it from the metadata service. Revoke it or let it expire after the first boot.
resource "openstack_identity_application_credential_v3" "authkey" {
  count       = var.authkey_from_secret ? 1 : 0
  name        = "${var.name}-authkey"
  description = "headlamp ${var.name}: read the tailscale auth key from Barbican at first boot"
  expires_at  = var.authkey_credential_expires_at

  access_rules {
    service = "key-manager"
    method  = "GET"
    path    = "/v1/secrets/${local.authkey_secret_id}/payload"
  }
}

resource "openstack_compute_keypair_v2" "this" {
  name       = "${var.name}-key"
  public_key = var.ssh_public_key
}

resource "openstack_networking_network_v2" "this" {
  name           = "${var.name}-net"
  admin_state_up = true
  tags           = local.tags
}

resource "openstack_networking_subnet_v2" "this" {
  name       = "${var.name}-subnet"
  network_id = openstack_networking_network_v2.this.id
  cidr       = var.subnet_cidr
  tags       = local.tags
}

resource "openstack_networking_router_v2" "this" {
  name                = "${var.name}-router"
  external_network_id = data.openstack_networking_network_v2.external.id
  tags                = local.tags
}

resource "openstack_networking_router_interface_v2" "this" {
  router_id = openstack_networking_router_v2.this.id
  subnet_id = openstack_networking_subnet_v2.this.id
}

# Inbound: tailscale's WireGuard UDP port, optional SSH, and the extra public ports of a bridge (#235). Outbound:
# neutron's default allow-all egress.
resource "openstack_networking_secgroup_v2" "this" {
  name        = "${var.name}-sg"
  description = "${var.advertise_exit_node ? "exit node" : "bridge"}: tailscale UDP in, everything out"
  tags        = local.tags
}

resource "openstack_networking_secgroup_rule_v2" "extra_tcp_in" {
  for_each          = toset([for p in var.extra_tcp_ingress_ports : tostring(p)])
  security_group_id = openstack_networking_secgroup_v2.this.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = tonumber(each.value)
  port_range_max    = tonumber(each.value)
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "openstack_networking_secgroup_rule_v2" "extra_udp_in" {
  for_each          = toset([for p in var.extra_udp_ingress_ports : tostring(p)])
  security_group_id = openstack_networking_secgroup_v2.this.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "udp"
  port_range_min    = tonumber(each.value)
  port_range_max    = tonumber(each.value)
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "openstack_networking_secgroup_rule_v2" "tailscale_in" {
  security_group_id = openstack_networking_secgroup_v2.this.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "udp"
  port_range_min    = var.tailscale_port
  port_range_max    = var.tailscale_port
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "openstack_networking_secgroup_rule_v2" "ssh_in" {
  count             = var.admin_ssh_cidr == null ? 0 : 1
  security_group_id = openstack_networking_secgroup_v2.this.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = var.admin_ssh_cidr
}

resource "openstack_networking_port_v2" "this" {
  name               = "${var.name}-port"
  network_id         = openstack_networking_network_v2.this.id
  security_group_ids = [openstack_networking_secgroup_v2.this.id]
  tags               = local.tags

  fixed_ip {
    subnet_id = openstack_networking_subnet_v2.this.id
  }
}

resource "openstack_blockstorage_volume_v3" "boot" {
  name                 = "${var.name}-boot"
  size                 = var.disk_size_gb
  image_id             = local.image_id
  volume_type          = var.volume_type
  availability_zone    = var.availability_zone
  enable_online_resize = true
  metadata             = local.metadata

  lifecycle {
    ignore_changes = [image_id]
  }
}

resource "openstack_compute_instance_v2" "this" {
  name              = var.name
  flavor_id         = var.flavor_id
  key_pair          = openstack_compute_keypair_v2.this.name
  availability_zone = var.availability_zone
  user_data         = local.user_data
  tags              = local.instance_tags
  metadata          = local.metadata

  network {
    port = openstack_networking_port_v2.this.id
  }

  block_device {
    uuid             = openstack_blockstorage_volume_v3.boot.id
    source_type      = "volume"
    destination_type = "volume"
    boot_index       = 0
  }

  vendor_options {
    ignore_resize_confirmation = true
  }

  # cloud-init (the auth key, the hostname) only runs on the first boot, so editing it must not silently recreate
  # a healthy server. Redeploy on purpose with: terraform apply -replace='module.<name>.openstack_compute_instance_v2.this'
  lifecycle {
    ignore_changes = [image_id, user_data]

    precondition {
      condition     = !var.bootstrap_tailscale || (var.headscale_login_server != "" && (var.authkey_from_secret || var.tailscale_authkey != ""))
      error_message = "bootstrap_tailscale = true needs headscale_login_server and tailscale_authkey (or authkey_from_secret = true)."
    }

    precondition {
      condition     = !var.authkey_from_secret || (var.bootstrap_tailscale && var.authkey_secret_ref != null && var.keystone_auth_url != null && var.tailscale_authkey == "")
      error_message = "authkey_from_secret = true needs bootstrap_tailscale = true, authkey_secret_ref and keystone_auth_url, and no tailscale_authkey."
    }
  }
}

resource "openstack_networking_floatingip_v2" "this" {
  pool = data.openstack_networking_network_v2.external.name
  tags = local.tags
}

resource "openstack_networking_floatingip_associate_v2" "this" {
  port_id     = openstack_networking_port_v2.this.id
  floating_ip = openstack_networking_floatingip_v2.this.address

  # Neutron refuses the association until the subnet is reachable from the external network, i.e. until the
  # router has the subnet attached (otherwise: ExternalGatewayForFloatingIPNotFound / 404).
  depends_on = [openstack_networking_router_interface_v2.this]
}

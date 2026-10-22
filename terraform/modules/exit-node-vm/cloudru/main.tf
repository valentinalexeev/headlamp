data "cloudru_evolution_compute_image_collection" "images" {
  project_id = var.project_id
}

data "cloudru_evolution_compute_disk_type_collection" "types" {
  project_id = var.project_id
}

locals {
  hostname   = coalesce(var.tailscale_hostname, var.name)
  zone       = { name = var.zone_name }
  images     = [for i in data.cloudru_evolution_compute_image_collection.images.images : i if i.name == var.image_name]
  disk_types = data.cloudru_evolution_compute_disk_type_collection.types.disk_types
  disk_type  = var.disk_type_name == null ? try(local.disk_types[0], null) : one([for t in local.disk_types : t if t.name == var.disk_type_name])
  port_range = "${var.tailscale_port}:${var.tailscale_port}"
  image_meta = var.image_metadata != null ? var.image_metadata : { public_key = { string_value = var.ssh_public_key } }
  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    bootstrap_tailscale    = var.bootstrap_tailscale
    headscale_login_server = var.headscale_login_server
    tailscale_authkey      = var.tailscale_authkey
    hostname               = local.hostname
  })
}

resource "cloudru_evolution_compute_subnet" "this" {
  project_id     = var.project_id
  zone           = local.zone
  name           = "${var.name}-subnet"
  vpc_id         = var.vpc_id
  subnet_address = var.subnet_address
}

# Inbound: only tailscale's WireGuard UDP port (and optionally SSH). Outbound: everything.
resource "cloudru_evolution_compute_security_group" "this" {
  project_id = var.project_id
  zone       = local.zone
  name       = "${var.name}-sg"
}

resource "cloudru_evolution_compute_security_group_rule" "tailscale_in" {
  security_group_id = cloudru_evolution_compute_security_group.this.id
  direction         = "TRAFFIC_DIRECTION_INGRESS"
  ether_type        = "ETHER_TYPE_IPV4"
  ip_protocol       = "IP_PROTOCOL_UDP"
  port_range        = local.port_range
  remote_ip_prefix  = "0.0.0.0/0"
  description       = "tailscale WireGuard"
}

resource "cloudru_evolution_compute_security_group_rule" "ssh_in" {
  count             = var.admin_ssh_cidr == null ? 0 : 1
  security_group_id = cloudru_evolution_compute_security_group.this.id
  direction         = "TRAFFIC_DIRECTION_INGRESS"
  ether_type        = "ETHER_TYPE_IPV4"
  ip_protocol       = "IP_PROTOCOL_TCP"
  port_range        = "22:22"
  remote_ip_prefix  = var.admin_ssh_cidr
  description       = "admin ssh"
}

resource "cloudru_evolution_compute_security_group_rule" "egress_tcp" {
  security_group_id = cloudru_evolution_compute_security_group.this.id
  direction         = "TRAFFIC_DIRECTION_EGRESS"
  ether_type        = "ETHER_TYPE_IPV4"
  ip_protocol       = "IP_PROTOCOL_TCP"
  port_range        = "1:65535"
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "cloudru_evolution_compute_security_group_rule" "egress_udp" {
  security_group_id = cloudru_evolution_compute_security_group.this.id
  direction         = "TRAFFIC_DIRECTION_EGRESS"
  ether_type        = "ETHER_TYPE_IPV4"
  ip_protocol       = "IP_PROTOCOL_UDP"
  port_range        = "1:65535"
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "cloudru_evolution_compute_external_ip" "this" {
  project_id = var.project_id
  zone       = local.zone
  name       = "${var.name}-ip"
}

resource "cloudru_evolution_compute_disk" "boot" {
  project_id = var.project_id
  zone       = local.zone
  name       = "${var.name}-boot"
  size       = var.disk_size_gb
  bootable   = true
  disk_type  = { id = local.disk_type.id }
  image      = { id = local.images[0].id }

  lifecycle {
    precondition {
      condition     = length(local.images) == 1
      error_message = "No unique image named '${var.image_name}' in the project (see the cloudru_evolution_compute_image_collection data source)."
    }
    precondition {
      condition     = local.disk_type != null
      error_message = "No matching disk type (see the cloudru_evolution_compute_disk_type_collection data source)."
    }
  }
}

resource "cloudru_evolution_compute_interface" "this" {
  project_id                 = var.project_id
  zone                       = local.zone
  name                       = "${var.name}-nic"
  subnet                     = { id = cloudru_evolution_compute_subnet.this.id }
  security_groups            = [{ id = cloudru_evolution_compute_security_group.this.id }]
  interface_security_enabled = true
  attach_external_ip         = { id = cloudru_evolution_compute_external_ip.this.id }
}

resource "cloudru_evolution_compute_vm" "this" {
  project_id          = var.project_id
  zone                = local.zone
  name                = var.name
  flavor              = { name = var.flavor_name }
  disks               = [{ id = cloudru_evolution_compute_disk.boot.id }]
  network_interfaces  = [{ id = cloudru_evolution_compute_interface.this.id }]
  image_metadata      = local.image_meta
  cloud_init_userdata = base64encode(local.user_data)

  # cloud-init (the auth key, the hostname) only runs on the first boot, so editing it must not silently
  # recreate a healthy VM. Redeploy on purpose with: terraform apply -replace='module.<name>.cloudru_evolution_compute_vm.this'
  lifecycle {
    ignore_changes = [cloud_init_userdata]

    precondition {
      condition     = !var.bootstrap_tailscale || (var.headscale_login_server != "" && var.tailscale_authkey != "")
      error_message = "bootstrap_tailscale = true needs headscale_login_server and tailscale_authkey."
    }
  }
}

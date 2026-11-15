locals {
  labels = merge(var.labels, { "headlamp-role" = "control-host" })
  # Public ports. 443/tcp: headscale (HTTPS, DERP), node-control enrolment and funnel-ingress, by name on the TLS
  # edge. 80/tcp: ACME HTTP-01 and the redirect to HTTPS. 3478/udp: STUN for the embedded DERP server.
  ports = [
    { port = 443, protocol = "tcp" },
    { port = 80, protocol = "tcp" },
    { port = 3478, protocol = "udp" },
  ]
  protocols   = toset([for p in local.ports : p.protocol])
  tag         = "${var.name}-host"
  dns_project = coalesce(var.dns_zone_project, var.project)
  # The device name of the data disk; the guest sees /dev/disk/by-id/google-<name>.
  data_device = "headlamp-data"
}

# --- the host's identity: no project role, only what the options grant

resource "google_service_account" "this" {
  project      = var.project
  account_id   = substr("${var.name}-host", 0, 30)
  display_name = "headlamp control host ${var.name}"
}

# ACME DNS-01 and the funnel record: change rights on one managed zone only.
resource "google_dns_managed_zone_iam_member" "dns01" {
  count        = var.dns_zone == null ? 0 : 1
  project      = local.dns_project
  managed_zone = var.dns_zone
  role         = "roles/dns.admin"
  member       = "serviceAccount:${google_service_account.this.email}"
}

# gcpsm:// references of node-control (#361): read access to each listed secret only.
resource "google_secret_manager_secret_iam_member" "read" {
  for_each  = toset(var.secrets)
  project   = split("/", each.value)[1]
  secret_id = split("/", each.value)[3]
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.this.email}"
}

# --- firewall: the public ports, and SSH only from admin_cidrs

resource "google_compute_firewall" "public" {
  project       = var.project
  name          = "${var.name}-host-public"
  network       = var.network
  direction     = "INGRESS"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = [local.tag]

  # The passthrough load balancer keeps the client address, so the rules are the same with and without it.
  dynamic "allow" {
    for_each = local.protocols
    content {
      protocol = allow.value
      ports    = [for p in local.ports : tostring(p.port) if p.protocol == allow.value]
    }
  }
}

resource "google_compute_firewall" "ssh" {
  count         = length(var.admin_cidrs) > 0 ? 1 : 0
  project       = var.project
  name          = "${var.name}-host-ssh"
  network       = var.network
  direction     = "INGRESS"
  source_ranges = var.admin_cidrs
  target_tags   = [local.tag]
  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# The load balancer checks the HTTPS port; Google's checkers come from these ranges.
resource "google_compute_firewall" "health" {
  count         = var.load_balancer ? 1 : 0
  project       = var.project
  name          = "${var.name}-host-health"
  network       = var.network
  direction     = "INGRESS"
  source_ranges = ["35.191.0.0/16", "209.85.152.0/22", "209.85.204.0/22"]
  target_tags   = [local.tag]
  allow {
    protocol = "tcp"
    ports    = ["443"]
  }
}

# --- the data disk, with daily snapshots

resource "google_compute_resource_policy" "snapshots" {
  count   = var.snapshot_retain_days > 0 ? 1 : 0
  project = var.project
  region  = var.region
  name    = "${var.name}-data-daily"
  snapshot_schedule_policy {
    schedule {
      daily_schedule {
        days_in_cycle = 1
        start_time    = "03:00"
      }
    }
    retention_policy {
      max_retention_days    = var.snapshot_retain_days
      on_source_disk_delete = "KEEP_AUTO_SNAPSHOTS"
    }
    snapshot_properties {
      labels = local.labels
    }
  }
}

resource "google_compute_disk" "data" {
  project  = var.project
  zone     = var.zone
  name     = "${var.name}-data"
  type     = var.data_disk_type
  size     = var.data_disk_gb
  snapshot = var.data_snapshot
  labels   = local.labels

  lifecycle {
    # The disk holds the tailnet: replacing the VM must never replace it.
    prevent_destroy = true
    # data_snapshot is for a restore into a new disk; changing it later must not replace this one.
    ignore_changes = [snapshot]
  }
}

resource "google_compute_disk_resource_policy_attachment" "snapshots" {
  count   = var.snapshot_retain_days > 0 ? 1 : 0
  project = var.project
  zone    = var.zone
  disk    = google_compute_disk.data.name
  name    = google_compute_resource_policy.snapshots[0].name
}

# --- the address: on the VM, or on the load balancer

resource "google_compute_address" "this" {
  project      = var.project
  region       = var.region
  name         = "${var.name}-host"
  address_type = "EXTERNAL"
  labels       = local.labels
}

# --- the host

resource "google_compute_instance" "this" {
  project      = var.project
  zone         = var.zone
  name         = var.name
  machine_type = var.machine_type
  tags         = [local.tag]
  labels       = local.labels

  boot_disk {
    auto_delete = true
    initialize_params {
      image  = var.image
      size   = var.boot_disk_gb
      labels = local.labels
    }
  }

  attached_disk {
    source      = google_compute_disk.data.id
    device_name = local.data_device
  }

  network_interface {
    network    = var.network
    subnetwork = var.subnetwork
    # An external address for egress (image pulls, ACME). Without a load balancer it is the static address.
    access_config {
      nat_ip = var.load_balancer ? null : google_compute_address.this.address
    }
  }

  service_account {
    email = google_service_account.this.email
    # The account has no project role, so the broad scope gives only what the options above grant.
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    user-data = templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
      device          = local.data_device
      user_data_extra = var.user_data_extra
    })
    enable-oslogin         = "TRUE"
    block-project-ssh-keys = "true"
  }

  allow_stopping_for_update = true

  lifecycle {
    # A new image does not replace a running host.
    ignore_changes = [boot_disk[0].initialize_params[0].image]
  }
}

# --- external passthrough network load balancer: TCP and UDP pass through, TLS ends on the host

resource "google_compute_instance_group" "this" {
  count     = var.load_balancer ? 1 : 0
  project   = var.project
  zone      = var.zone
  name      = "${var.name}-host"
  instances = [google_compute_instance.this.self_link]
}

resource "google_compute_region_health_check" "this" {
  count   = var.load_balancer ? 1 : 0
  project = var.project
  region  = var.region
  name    = "${var.name}-host"
  tcp_health_check {
    port = 443
  }
}

resource "google_compute_region_backend_service" "this" {
  for_each              = var.load_balancer ? local.protocols : toset([])
  project               = var.project
  region                = var.region
  name                  = "${var.name}-host-${each.key}"
  protocol              = upper(each.key)
  load_balancing_scheme = "EXTERNAL"
  # UDP backends cannot be checked over UDP; both backends check the HTTPS port over TCP.
  health_checks = [google_compute_region_health_check.this[0].id]
  backend {
    group          = google_compute_instance_group.this[0].self_link
    balancing_mode = "CONNECTION"
  }
}

resource "google_compute_forwarding_rule" "this" {
  for_each              = var.load_balancer ? local.protocols : toset([])
  project               = var.project
  region                = var.region
  name                  = "${var.name}-host-${each.key}"
  ip_address            = google_compute_address.this.address
  ip_protocol           = upper(each.key)
  ports                 = [for p in local.ports : tostring(p.port) if p.protocol == each.key]
  load_balancing_scheme = "EXTERNAL"
  backend_service       = google_compute_region_backend_service.this[each.key].id
  labels                = local.labels
}

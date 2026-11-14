locals {
  hostname   = coalesce(var.tailscale_hostname, var.name)
  sa_id      = coalesce(var.service_account_id, "${var.name}-exit-node")
  use_secret = var.authkey_from_secret
  # The label that node-control requires before it starts or stops a VM. On the instance only (#226).
  instance_labels = merge(var.labels, { "headlamp-exit-node" = "true" })
  # The secret's project and id; "-" when unset, so the check below reports a missing secret, not a split error.
  secret_parts = split("/", coalesce(var.authkey_secret, "projects/-/secrets/-"))
  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
    use_secret             = local.use_secret
    fetch_secret_b64       = base64encode(file("${path.module}/templates/fetch-secret.py"))
    authkey_secret         = coalesce(var.authkey_secret, "-")
    tailscale_authkey      = var.tailscale_authkey == null ? "" : var.tailscale_authkey
    tailscale_version      = var.tailscale_version
    headscale_login_server = var.headscale_login_server
    hostname               = local.hostname
  })
}

check "one_auth_key_source" {
  assert {
    condition     = var.authkey_from_secret != (var.tailscale_authkey != null)
    error_message = "Set exactly one of authkey_from_secret = true (with authkey_secret) or tailscale_authkey."
  }
}

check "secret_name_with_switch" {
  assert {
    condition     = !var.authkey_from_secret || var.authkey_secret != null
    error_message = "authkey_from_secret = true needs authkey_secret = \"projects/<project>/secrets/<name>\"."
  }
}

# The node's own identity: no project role. It may read only its auth key secret (#214).
resource "google_service_account" "this" {
  project      = var.project
  account_id   = local.sa_id
  display_name = "headlamp exit node ${var.name}"
}

# #223: count follows the plan-time bool, never the secret's value, so the secret can be made in the same stack.
resource "google_secret_manager_secret_iam_member" "authkey" {
  count     = var.authkey_from_secret ? 1 : 0
  project   = local.secret_parts[1]
  secret_id = local.secret_parts[3]
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.this.email}"
}

# WireGuard only. No SSH: admin access is over the tailnet.
resource "google_compute_firewall" "wireguard" {
  project       = var.project
  name          = "${var.name}-exit-node-wg"
  network       = var.network
  direction     = "INGRESS"
  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["headlamp-exit-node"]
  allow {
    protocol = "udp"
    ports    = ["41641"]
  }
}

resource "google_compute_instance" "this" {
  project      = var.project
  zone         = var.zone
  name         = var.name
  machine_type = var.machine_type
  tags         = ["headlamp-exit-node"]
  labels       = local.instance_labels

  # An exit node forwards traffic for other hosts.
  can_ip_forward = true

  boot_disk {
    # A stop keeps the disk, and with it the tailscaled state: the same headscale node after a start (#214).
    auto_delete = true
    initialize_params {
      image  = var.image
      size   = var.disk_size_gb
      labels = var.labels
    }
  }

  network_interface {
    network    = var.network
    subnetwork = var.subnetwork
    access_config {}
  }

  service_account {
    email = google_service_account.this.email
    # The account has no project role, so the broad scope gives nothing beyond its secret binding.
    scopes = ["cloud-platform"]
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  metadata = {
    user-data              = local.user_data
    enable-oslogin         = "TRUE"
    block-project-ssh-keys = "true"
  }

  scheduling {
    provisioning_model          = var.spot ? "SPOT" : "STANDARD"
    preemptible                 = var.spot
    automatic_restart           = !var.spot
    instance_termination_action = var.spot ? "STOP" : null
  }

  # node-control stops and starts the VM; Terraform must not fight over its power state.
  desired_status = null
  lifecycle {
    ignore_changes = [desired_status]
  }

  depends_on = [google_secret_manager_secret_iam_member.authkey]
}

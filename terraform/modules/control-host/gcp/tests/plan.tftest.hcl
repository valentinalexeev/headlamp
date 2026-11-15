# Plan-only checks with a mocked Google provider: no project and no credentials needed.
#   cd terraform/modules/control-host/gcp && terraform init -backend=false && terraform test
mock_provider "google" {
  override_during = plan
  mock_resource "google_service_account" {
    defaults = { email = "c1-host@p1.iam.gserviceaccount.com" }
  }
  mock_resource "google_compute_address" {
    defaults = { address = "203.0.113.10" }
  }
  mock_resource "google_compute_disk" {
    defaults = { id = "projects/p1/zones/europe-west3-a/disks/c1-data" }
  }
  mock_resource "google_compute_region_health_check" {
    defaults = { id = "projects/p1/regions/europe-west3/healthChecks/c1-host" }
  }
  mock_resource "google_compute_instance_group" {
    defaults = { self_link = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west3-a/instanceGroups/c1-host" }
  }
  mock_resource "google_compute_region_backend_service" {
    defaults = { id = "projects/p1/regions/europe-west3/backendServices/c1-host" }
  }
  mock_resource "google_compute_instance" {
    defaults = { self_link = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west3-a/instances/c1" }
  }
}

variables {
  name    = "c1"
  project = "p1"
  region  = "europe-west3"
  zone    = "europe-west3-a"
  labels  = { team = "net" }
}

run "defaults_static_address_no_ssh_no_extra_rights" {
  command = plan

  assert {
    condition     = google_compute_instance.this.network_interface[0].access_config[0].nat_ip == "203.0.113.10"
    error_message = "without a load balancer the static address is on the VM"
  }
  assert {
    condition     = length(google_compute_forwarding_rule.this) == 0 && length(google_compute_instance_group.this) == 0 && length(google_compute_firewall.health) == 0
    error_message = "no load balancer by default"
  }
  assert {
    condition     = length(google_compute_firewall.ssh) == 0
    error_message = "no SSH rule without admin_cidrs"
  }
  assert {
    condition     = length(google_dns_managed_zone_iam_member.dns01) == 0 && length(google_secret_manager_secret_iam_member.read) == 0
    error_message = "the service account has no rights unless an option grants them"
  }
  assert {
    condition     = google_compute_instance.this.metadata["enable-oslogin"] == "TRUE" && google_compute_instance.this.metadata["block-project-ssh-keys"] == "true"
    error_message = "OS Login on, project SSH keys blocked"
  }
  assert {
    condition     = google_compute_instance.this.shielded_instance_config[0].enable_secure_boot
    error_message = "Shielded VM"
  }
  assert {
    condition     = google_compute_instance.this.labels["headlamp-role"] == "control-host" && google_compute_instance.this.labels["team"] == "net"
    error_message = "labels"
  }
}

run "public_firewall_has_the_three_ports_only" {
  command = plan

  assert {
    condition = (
      contains(google_compute_firewall.public.target_tags, "c1-host") &&
      length(google_compute_firewall.public.allow) == 2 &&
      toset(flatten([for a in google_compute_firewall.public.allow : [for p in a.ports : "${p}/${a.protocol}"]])) == toset(["443/tcp", "80/tcp", "3478/udp"])
    )
    error_message = "the public rule must allow 443/tcp, 80/tcp and 3478/udp, and nothing else"
  }
  assert {
    condition     = google_compute_firewall.public.source_ranges == toset(["0.0.0.0/0"])
    error_message = "public ports are open to all"
  }
}

run "data_disk_is_separate_mounted_and_snapshotted" {
  command = plan

  assert {
    condition     = google_compute_instance.this.attached_disk[0].device_name == "headlamp-data" && google_compute_disk.data.size == 10 && google_compute_disk.data.type == "pd-balanced"
    error_message = "the data disk is attached under the device name the mount script reads"
  }
  assert {
    condition     = strcontains(google_compute_instance.this.metadata["user-data"], "/dev/disk/by-id/google-headlamp-data") && strcontains(google_compute_instance.this.metadata["user-data"], "blkid \"$dev\" >/dev/null 2>&1 || mkfs.ext4")
    error_message = "cloud-init formats the disk only when it has no file system"
  }
  assert {
    condition     = google_compute_resource_policy.snapshots[0].snapshot_schedule_policy[0].retention_policy[0].max_retention_days == 14 && google_compute_disk_resource_policy_attachment.snapshots[0].disk == "c1-data"
    error_message = "daily snapshots, kept 14 days, on the data disk"
  }
  assert {
    condition     = google_compute_resource_policy.snapshots[0].snapshot_schedule_policy[0].retention_policy[0].on_source_disk_delete == "KEEP_AUTO_SNAPSHOTS"
    error_message = "snapshots outlive the disk"
  }
}

run "snapshots_off" {
  command = plan
  variables {
    snapshot_retain_days = 0
  }
  assert {
    condition     = length(google_compute_resource_policy.snapshots) == 0 && length(google_compute_disk_resource_policy_attachment.snapshots) == 0
    error_message = "0 turns snapshots off"
  }
}

run "restore_from_snapshot" {
  command = plan
  variables {
    data_snapshot = "projects/p1/global/snapshots/c1-data-20261115"
    data_disk_gb  = 30
  }
  assert {
    condition     = google_compute_disk.data.snapshot == "projects/p1/global/snapshots/c1-data-20261115" && google_compute_disk.data.size == 30
    error_message = "the data disk is created from the snapshot"
  }
}

run "load_balancer_passes_the_ports_through" {
  command = plan
  variables {
    load_balancer = true
  }

  assert {
    condition     = google_compute_instance.this.network_interface[0].access_config[0].nat_ip != "203.0.113.10"
    error_message = "with a load balancer the VM gets an ephemeral address, and the static one is on the forwarding rules"
  }
  assert {
    condition = (
      google_compute_forwarding_rule.this["tcp"].ports == toset(["443", "80"]) &&
      google_compute_forwarding_rule.this["udp"].ports == toset(["3478"]) &&
      google_compute_forwarding_rule.this["tcp"].ip_address == "203.0.113.10" &&
      google_compute_forwarding_rule.this["udp"].ip_address == "203.0.113.10" &&
      google_compute_forwarding_rule.this["tcp"].load_balancing_scheme == "EXTERNAL"
    )
    error_message = "one external address, TCP 443 and 80 and UDP 3478"
  }
  assert {
    condition     = google_compute_region_backend_service.this["udp"].protocol == "UDP" && google_compute_region_health_check.this[0].tcp_health_check[0].port == 443
    error_message = "UDP backend, checked over TCP 443"
  }
  assert {
    condition     = one(google_compute_firewall.health[0].allow).ports == tolist(["443"]) && contains(google_compute_firewall.health[0].source_ranges, "35.191.0.0/16")
    error_message = "the health checkers reach 443"
  }
}

run "ssh_from_admin_cidrs_only" {
  command = plan
  variables {
    admin_cidrs = ["35.235.240.0/20"]
  }
  assert {
    condition     = google_compute_firewall.ssh[0].source_ranges == toset(["35.235.240.0/20"]) && one(google_compute_firewall.ssh[0].allow).ports == tolist(["22"])
    error_message = "SSH from the listed ranges only"
  }
}

run "dns_admin_on_one_zone" {
  command = plan
  variables {
    dns_zone         = "example-com"
    dns_zone_project = "p-dns"
  }
  assert {
    condition = (
      google_dns_managed_zone_iam_member.dns01[0].managed_zone == "example-com" &&
      google_dns_managed_zone_iam_member.dns01[0].project == "p-dns" &&
      google_dns_managed_zone_iam_member.dns01[0].role == "roles/dns.admin" &&
      google_dns_managed_zone_iam_member.dns01[0].member == "serviceAccount:c1-host@p1.iam.gserviceaccount.com"
    )
    error_message = "the host may change that one zone only"
  }
}

run "dns_zone_project_defaults_to_project" {
  command = plan
  variables {
    dns_zone = "example-com"
  }
  assert {
    condition     = google_dns_managed_zone_iam_member.dns01[0].project == "p1"
    error_message = "the zone is in the host's project by default"
  }
}

run "secret_reader_per_secret" {
  command = plan
  variables {
    secrets = ["projects/p1/secrets/hs-api-key", "projects/p2/secrets/state-key"]
  }
  assert {
    condition = (
      length(google_secret_manager_secret_iam_member.read) == 2 &&
      google_secret_manager_secret_iam_member.read["projects/p2/secrets/state-key"].project == "p2" &&
      google_secret_manager_secret_iam_member.read["projects/p2/secrets/state-key"].secret_id == "state-key" &&
      google_secret_manager_secret_iam_member.read["projects/p1/secrets/hs-api-key"].role == "roles/secretmanager.secretAccessor"
    )
    error_message = "read access to each listed secret only"
  }
}

run "bad_secret_is_refused" {
  command = plan
  variables {
    secrets = ["hs-api-key"]
  }
  expect_failures = [var.secrets]
}

run "bad_name_is_refused" {
  command = plan
  variables {
    name = "Control_1"
  }
  expect_failures = [var.name]
}

run "machine_image_network_and_extra_commands" {
  command = plan
  variables {
    machine_type    = "e2-medium"
    image           = "debian-cloud/debian-13"
    boot_disk_gb    = 30
    data_disk_type  = "pd-ssd"
    network         = "vpc1"
    subnetwork      = "sub1"
    user_data_extra = "git clone https://example.com/site.git /srv/headlamp/compose"
  }
  assert {
    condition = (
      google_compute_instance.this.machine_type == "e2-medium" &&
      google_compute_instance.this.boot_disk[0].initialize_params[0].image == "debian-cloud/debian-13" &&
      google_compute_instance.this.boot_disk[0].initialize_params[0].size == 30 &&
      google_compute_disk.data.type == "pd-ssd" &&
      google_compute_instance.this.network_interface[0].network == "vpc1" &&
      google_compute_instance.this.network_interface[0].subnetwork == "sub1" &&
      google_compute_firewall.public.network == "vpc1"
    )
    error_message = "machine, image, disks and network follow the inputs"
  }
  assert {
    condition     = strcontains(google_compute_instance.this.metadata["user-data"], "    git clone https://example.com/site.git /srv/headlamp/compose")
    error_message = "user_data_extra runs after the mount"
  }
}

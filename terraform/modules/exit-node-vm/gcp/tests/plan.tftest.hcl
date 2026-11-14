# Plan-only checks with a mocked Google provider: no project and no credentials needed.
#   cd terraform/modules/exit-node-vm/gcp && terraform init -backend=false && terraform test
mock_provider "google" {
  mock_resource "google_service_account" {
    defaults = { email = "n1-exit-node@p1.iam.gserviceaccount.com" }
  }
}

variables {
  name                   = "n1"
  zone                   = "europe-west3-a"
  project                = "p1"
  headscale_login_server = "https://hs.example.com"
  labels                 = { team = "net" }
}

run "secret_mode_keeps_the_key_out_of_metadata" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
  }

  assert {
    condition     = !strcontains(google_compute_instance.this.metadata["user-data"], "tskey")
    error_message = "the auth key must not be in the metadata"
  }
  assert {
    condition     = strcontains(google_compute_instance.this.metadata["user-data"], "headlamp-fetch-secret 'projects/p1/secrets/n1-authkey'")
    error_message = "the node must read the key from its secret"
  }
  assert {
    condition     = length(google_secret_manager_secret_iam_member.authkey) == 1 && google_secret_manager_secret_iam_member.authkey[0].secret_id == "n1-authkey" && google_secret_manager_secret_iam_member.authkey[0].project == "p1" && google_secret_manager_secret_iam_member.authkey[0].role == "roles/secretmanager.secretAccessor"
    error_message = "the service account must read that one secret only"
  }
}

run "spot_stops_on_preemption" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
  }

  assert {
    condition     = google_compute_instance.this.scheduling[0].provisioning_model == "SPOT" && google_compute_instance.this.scheduling[0].instance_termination_action == "STOP" && google_compute_instance.this.scheduling[0].automatic_restart == false
    error_message = "a preempted Spot VM must be stopped, not deleted, so the node keeps its identity"
  }
}

run "standard_vm_restarts" {
  command = plan
  variables {
    spot                = false
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
  }

  assert {
    condition     = google_compute_instance.this.scheduling[0].provisioning_model == "STANDARD" && google_compute_instance.this.scheduling[0].automatic_restart == true && google_compute_instance.this.scheduling[0].preemptible == false
    error_message = "spot = false must be a standard VM"
  }
}

run "labels_on_the_disk_and_control_label_on_the_instance" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
  }

  assert {
    condition     = google_compute_instance.this.labels["headlamp-exit-node"] == "true" && google_compute_instance.this.labels["team"] == "net"
    error_message = "the instance needs the site labels and the control label"
  }
  assert {
    condition     = google_compute_instance.this.boot_disk[0].initialize_params[0].labels["team"] == "net" && !contains(keys(google_compute_instance.this.boot_disk[0].initialize_params[0].labels), "headlamp-exit-node")
    error_message = "the boot disk needs the site labels (#226), not the control label"
  }
}

run "hardened_instance" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
  }

  assert {
    condition     = google_compute_instance.this.shielded_instance_config[0].enable_secure_boot && google_compute_instance.this.shielded_instance_config[0].enable_vtpm && google_compute_instance.this.shielded_instance_config[0].enable_integrity_monitoring
    error_message = "the VM must be a Shielded VM"
  }
  assert {
    condition     = google_compute_instance.this.metadata["enable-oslogin"] == "TRUE" && google_compute_instance.this.metadata["block-project-ssh-keys"] == "true"
    error_message = "OS Login on and project SSH keys blocked"
  }
  assert {
    condition     = length(google_compute_firewall.wireguard.allow) == 1 && one(google_compute_firewall.wireguard.allow).protocol == "udp" && one(google_compute_firewall.wireguard.allow).ports == tolist(["41641"])
    error_message = "the firewall must open WireGuard (41641/udp) only"
  }
}

run "restart_does_not_register_again" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
    tailscale_version   = "1.104.5"
  }

  assert {
    condition     = strcontains(google_compute_instance.this.metadata["user-data"], "if /usr/local/sbin/headlamp-logged-in; then exit 0; fi")
    error_message = "tailscale up must run only while the node is not logged in"
  }
  assert {
    condition     = strcontains(google_compute_instance.this.metadata["user-data"], "tailscale=1.104.5")
    error_message = "the pinned version must be installed"
  }
}

run "inline_key_still_works" {
  command = plan
  variables {
    tailscale_authkey = "tskey-inline-0000"
  }

  assert {
    condition     = length(google_secret_manager_secret_iam_member.authkey) == 0
    error_message = "no secret binding without authkey_from_secret"
  }
}

run "both_key_sources_are_refused" {
  command = plan
  variables {
    authkey_from_secret = true
    authkey_secret      = "projects/p1/secrets/n1-authkey"
    tailscale_authkey   = "tskey-inline-0000"
  }
  expect_failures = [check.one_auth_key_source]
}

run "switch_without_secret_is_refused" {
  command = plan
  variables {
    authkey_from_secret = true
  }
  expect_failures = [check.secret_name_with_switch]
}

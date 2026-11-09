# Plan-only checks with a mocked openstack provider: no cloud account needed.
#   cd terraform/modules/exit-node-vm/openstack && terraform init -backend=false && terraform test
mock_provider "openstack" {
  mock_data "openstack_images_image_v2" {
    defaults = { id = "img-ubuntu" }
  }
  mock_data "openstack_networking_network_v2" {
    defaults = { id = "net-ext", name = "external-network" }
  }
}

variables {
  name              = "node-b"
  availability_zone = "zone-a"
  flavor_id         = "1011"
  volume_type       = "fast.zone-a"
  ssh_public_key    = "ssh-ed25519 AAAAexample example"
}

run "defaults_are_an_exit_node_with_only_the_tailscale_port" {
  command = plan

  assert {
    condition     = length(openstack_networking_secgroup_rule_v2.extra_tcp_in) == 0 && length(openstack_networking_secgroup_rule_v2.extra_udp_in) == 0
    error_message = "the default must open only the tailscale UDP port"
  }
  assert {
    condition     = openstack_blockstorage_volume_v3.boot.image_id == "img-ubuntu" && length(data.openstack_images_image_v2.this) == 1
    error_message = "the default must look the image up by name"
  }
}

run "image_id_pins_the_image_and_skips_the_lookup" {
  command = plan
  variables {
    image_id = "img-debian-13-pinned"
  }

  assert {
    condition     = openstack_blockstorage_volume_v3.boot.image_id == "img-debian-13-pinned" && length(data.openstack_images_image_v2.this) == 0
    error_message = "image_id must set the boot volume image and skip the name lookup"
  }
}

run "bridge_ports_80_443_tcp_and_443_udp" {
  command = plan
  variables {
    extra_tcp_ingress_ports = [80, 443]
    extra_udp_ingress_ports = [443]
  }

  assert {
    condition     = toset(keys(openstack_networking_secgroup_rule_v2.extra_tcp_in)) == toset(["80", "443"])
    error_message = "the bridge must open 80 and 443 tcp"
  }
  assert {
    condition     = alltrue([for r in values(openstack_networking_secgroup_rule_v2.extra_tcp_in) : r.protocol == "tcp" && r.port_range_min == r.port_range_max && r.remote_ip_prefix == "0.0.0.0/0" && r.direction == "ingress"])
    error_message = "each tcp rule must be one port, inbound from anywhere"
  }
  assert {
    condition     = keys(openstack_networking_secgroup_rule_v2.extra_udp_in) == ["443"] && openstack_networking_secgroup_rule_v2.extra_udp_in["443"].protocol == "udp"
    error_message = "the bridge must open 443 udp for Hysteria 2"
  }
}

run "rejects_ssh_in_the_public_tcp_ports" {
  command = plan
  variables {
    extra_tcp_ingress_ports = [22]
  }
  expect_failures = [var.extra_tcp_ingress_ports]
}

run "rejects_a_udp_port_out_of_range" {
  command = plan
  variables {
    extra_udp_ingress_ports = [70000]
  }
  expect_failures = [var.extra_udp_ingress_ports]
}

run "exit_node_bootstrap_advertises_exit" {
  command = plan
  variables {
    bootstrap_tailscale    = true
    headscale_login_server = "https://hs.example.com"
    tailscale_authkey      = "tskey-0000"
  }

  assert {
    condition     = strcontains(openstack_compute_instance_v2.this.user_data, "--advertise-exit-node")
    error_message = "an exit node bootstrap must advertise the exit node"
  }
}

run "bridge_bootstrap_does_not_advertise_exit" {
  command = plan
  variables {
    bootstrap_tailscale    = true
    advertise_exit_node    = false
    headscale_login_server = "https://hs.example.com"
    tailscale_authkey      = "tskey-0000"
  }

  assert {
    condition     = !strcontains(openstack_compute_instance_v2.this.user_data, "advertise-exit-node") && strcontains(openstack_compute_instance_v2.this.user_data, "--hostname=node-b")
    error_message = "a bridge bootstrap must not advertise exit routes"
  }
}

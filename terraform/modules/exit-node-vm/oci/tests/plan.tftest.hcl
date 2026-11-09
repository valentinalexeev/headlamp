# Plan-only checks with a mocked oci provider: no cloud account needed.
#   cd terraform/modules/exit-node-vm/oci && terraform init -backend=false && terraform test
mock_provider "oci" {
  mock_data "oci_identity_availability_domains" {
    defaults = { availability_domains = [{ name = "AD-1", id = "ad1", compartment_id = "c" }] }
  }
  mock_data "oci_core_images" {
    defaults = { images = [{ id = "ocid1.image.oc1..ubuntu" }] }
  }
}

variables {
  name                   = "node-o"
  compartment_id         = "ocid1.compartment.oc1..x"
  ssh_public_key         = "ssh-ed25519 AAAAexample example"
  headscale_login_server = "https://hs.example.com"
  tailscale_authkey      = "tskey-0000"
}

run "default_opens_only_the_tailscale_udp_port" {
  command = plan

  assert {
    condition     = length(oci_core_security_list.this.ingress_security_rules) == 1
    error_message = "the default must open only the tailscale UDP port"
  }
}

run "bridge_tcp_80_443_and_udp_443" {
  command = apply

  variables {
    extra_tcp_ingress_ports = [80, 443]
    extra_udp_ingress_ports = [443]
  }

  assert {
    condition     = length(oci_core_security_list.this.ingress_security_rules) == 4
    error_message = "tailscale + 80/tcp + 443/tcp + 443/udp must be four rules"
  }
  assert {
    condition     = toset([for r in oci_core_security_list.this.ingress_security_rules : "${r.protocol}:${r.protocol == "17" ? r.udp_options[0].min : r.tcp_options[0].min}"]) == toset(["17:41641", "6:80", "6:443", "17:443"])
    error_message = "the rules must be 41641/udp, 80/tcp, 443/tcp and 443/udp"
  }
}

run "rejects_a_udp_port_out_of_range" {
  command = plan
  variables {
    extra_udp_ingress_ports = [0]
  }
  expect_failures = [var.extra_udp_ingress_ports]
}

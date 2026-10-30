data "oci_identity_availability_domains" "ads" {
  compartment_id = var.compartment_id
}

# OCI has no AWS-style "default VPC" -- build the minimal equivalent: one
# VCN, one public subnet, an internet gateway, and a route table sending
# 0.0.0.0/0 out through it.
resource "oci_core_vcn" "this" {
  compartment_id = var.compartment_id
  display_name   = "${var.name}-vcn"
  cidr_blocks    = ["10.20.0.0/16"]
  dns_label      = replace(var.name, "-", "")
}

resource "oci_core_internet_gateway" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-igw"
}

resource "oci_core_route_table" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-rt"

  route_rules {
    destination       = "0.0.0.0/0"
    network_entity_id = oci_core_internet_gateway.this.id
  }
}

# No public SSH ingress, deliberately -- matches the other VM modules'
# original AWS module: admin access happens over the tailnet once the node
# joins, tunneled inside the WireGuard UDP flow below, which this security
# list never inspects at the TCP:22 level. See the AWS module's git history
# for why this convention exists.
resource "oci_core_security_list" "this" {
  compartment_id = var.compartment_id
  vcn_id         = oci_core_vcn.this.id
  display_name   = "${var.name}-exit-node"

  ingress_security_rules {
    protocol    = "17" # UDP
    source      = "0.0.0.0/0"
    description = "Tailscale WireGuard"

    udp_options {
      min = 41641
      max = 41641
    }
  }

  dynamic "ingress_security_rules" {
    for_each = toset(var.extra_tcp_ingress_ports)
    content {
      protocol    = "6" # TCP
      source      = "0.0.0.0/0"
      description = "Extra TCP ${ingress_security_rules.value}"

      tcp_options {
        min = ingress_security_rules.value
        max = ingress_security_rules.value
      }
    }
  }

  egress_security_rules {
    protocol    = "all"
    destination = "0.0.0.0/0"
  }
}

resource "oci_core_subnet" "this" {
  compartment_id             = var.compartment_id
  vcn_id                     = oci_core_vcn.this.id
  display_name               = "${var.name}-subnet"
  cidr_block                 = "10.20.0.0/24"
  dns_label                  = "sub"
  route_table_id             = oci_core_route_table.this.id
  security_list_ids          = [oci_core_security_list.this.id]
  prohibit_public_ip_on_vnic = false
}

# shape-compatible: OCI's own filtering returns arm64 images for A1.Flex
# and x86_64 images for E2/E3/E4-family shapes without us tracking
# architecture explicitly (unlike AWS, which needs an explicit ami_arch).
data "oci_core_images" "ubuntu" {
  compartment_id           = var.compartment_id
  operating_system         = "Canonical Ubuntu"
  operating_system_version = "24.04"
  shape                    = var.shape
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

locals {
  # Only *.Flex shapes take an explicit ocpus/memory_in_gbs shape_config
  # block -- fixed shapes like VM.Standard.E2.1.Micro have implicit fixed
  # specs and error if one is supplied.
  is_flex_shape = can(regex("Flex$", var.shape))
}

resource "oci_core_instance" "this" {
  compartment_id      = var.compartment_id
  availability_domain = data.oci_identity_availability_domains.ads.availability_domains[0].name
  display_name        = var.name
  shape               = var.shape

  dynamic "shape_config" {
    for_each = local.is_flex_shape ? [1] : []
    content {
      ocpus         = var.ocpus
      memory_in_gbs = var.memory_in_gbs
    }
  }

  create_vnic_details {
    subnet_id        = oci_core_subnet.this.id
    assign_public_ip = true
  }

  source_details {
    source_type = "image"
    source_id   = data.oci_core_images.ubuntu.images[0].id
  }

  metadata = {
    ssh_authorized_keys = var.ssh_public_key
    user_data = base64encode(templatefile("${path.module}/templates/cloud-init.yaml.tmpl", {
      headscale_login_server = var.headscale_login_server
      tailscale_authkey      = var.tailscale_authkey
      hostname               = coalesce(var.tailscale_hostname, var.name)
    }))
  }

  # cloud-init (the auth key, the hostname) only runs on the VM's FIRST boot, so editing it must not
  # silently recreate a healthy instance. To redeploy on purpose (e.g. with a fresh key), run
  #   terraform apply -replace='module.<name>.oci_core_instance.this'
  lifecycle {
    ignore_changes = [metadata["user_data"]]
  }
}

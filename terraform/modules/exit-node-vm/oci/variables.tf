variable "name" {
  type        = string
  description = "Node name (instance name); also the tailscale hostname unless `tailscale_hostname` is set."
}

variable "tailscale_hostname" {
  type        = string
  default     = null
  description = "Hostname the node registers under on the tailnet. Defaults to `name`. Set it when the tailnet name differs from the instance name, so a replacement registers correctly without a manual rename."
}

variable "compartment_id" {
  type        = string
  description = "OCID of the OCI compartment to create resources in (see docs/superpowers/specs/ for the headlamp-scoped compartment setup)."
}

variable "shape" {
  type        = string
  default     = "VM.Standard.A1.Flex"
  description = "OCI compute shape. *.Flex shapes use ocpus/memory_in_gbs below; fixed shapes (e.g. VM.Standard.E2.1.Micro) ignore them."
}

variable "ocpus" {
  type    = number
  default = 1
}

variable "memory_in_gbs" {
  type    = number
  default = 1
}

variable "ssh_public_key" {
  type = string
}

variable "headscale_login_server" {
  type = string
}

variable "tailscale_authkey" {
  type      = string
  sensitive = true
}

variable "extra_tcp_ingress_ports" {
  type        = list(number)
  default     = []
  description = "Extra TCP ports to open inbound from anywhere in the security list (for a node that also serves a public front). Empty keeps the default: only the Tailscale UDP port."

  validation {
    condition     = alltrue([for p in var.extra_tcp_ingress_ports : p >= 1 && p <= 65535 && p == floor(p) && p != 22])
    error_message = "extra_tcp_ingress_ports: whole numbers 1-65535, and never 22 (admin access is over the tailnet only)."
  }
}

variable "extra_udp_ingress_ports" {
  type        = list(number)
  default     = []
  description = "Extra UDP ports to open inbound from anywhere in the security list, for a bridge with Hysteria 2 (443, #235). Empty keeps the default: only the Tailscale UDP port."

  validation {
    condition     = alltrue([for p in var.extra_udp_ingress_ports : p >= 1 && p <= 65535 && p == floor(p)])
    error_message = "extra_udp_ingress_ports: whole numbers 1-65535."
  }
}

variable "ubuntu_version" {
  type        = string
  default     = "26.04 Minimal"
  description = "OCI operating_system_version of the \"Canonical Ubuntu\" platform image, for example \"26.04 Minimal\", \"24.04 Minimal\" or \"24.04\" (full server image). The newest image of that version is picked at creation. An existing instance keeps its image: a change does not replace it, use terraform -replace for a deliberate rebuild."
}

variable "ubuntu_version_fallback" {
  type        = string
  default     = "24.04 Minimal"
  description = "operating_system_version to use when OCI has no image of ubuntu_version for the shape. Null turns the fallback off, so a missing image fails the plan."
}

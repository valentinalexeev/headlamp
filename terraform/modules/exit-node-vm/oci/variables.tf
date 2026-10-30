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

variable "name" {
  type        = string
  description = "Node name (server name); also the tailscale hostname unless `tailscale_hostname` is set."
}

variable "tailscale_hostname" {
  type        = string
  default     = null
  description = "Hostname the node registers under on the tailnet. Defaults to `name`."
}

variable "availability_zone" {
  type        = string
  description = "Pool segment, e.g. \"<zone>\". The boot volume is created in the same zone."
}

variable "flavor_id" {
  type        = string
  description = "Flavor ID of a fixed configuration (Selectel uses numeric IDs, see the flavor table in the docs)."
}

variable "volume_type" {
  type        = string
  description = "Boot volume type for the zone, e.g. \"<volume-type>\"."
}

variable "disk_size_gb" {
  type        = number
  default     = 10
  description = "Boot volume size in GB."
}

variable "image_name" {
  type        = string
  default     = "Ubuntu 22.04 LTS 64-bit"
  description = "Public image name, matched exactly (most recent)."
}

variable "subnet_cidr" {
  type        = string
  default     = "192.168.199.0/24"
  description = "CIDR of the private subnet this module creates (behind a router with the external network)."
}

variable "ssh_public_key" {
  type        = string
  description = "SSH public key for the key pair attached to the server."
}

variable "admin_ssh_cidr" {
  type        = string
  default     = null
  description = "If set, allow SSH (tcp/22) from this CIDR. Default: no public SSH, like the other VM modules."
}

variable "tailscale_port" {
  type        = number
  default     = 41641
  description = "UDP port opened for tailscale's WireGuard traffic."
}

variable "preemptible" {
  type        = bool
  default     = false
  description = <<-EOT
    Tag the server `preemptible` (about 70% cheaper). Selectel stops preemptible servers at the latest 24 hours
    after creation (state EXPIRED; restore with `openstack server unshelve`), so this suits throwaway nodes only
    unless you automate the restore. UNVERIFIED through Terraform: Selectel documents the tag with compute API
    microversion 2.72 while this provider requests 2.52 for tags; check the server's tags after the first apply
    and, if the tag is missing, set it with the OpenStack CLI (see README.md).
  EOT
}

variable "bootstrap_tailscale" {
  type        = bool
  default     = false
  description = <<-EOT
    Install tailscale and join the tailnet from cloud-init on first boot. Off by default because hosts in restricted networks
    often cannot reach tailscale.com / pkgs.tailscale.com reliably: leave it off and let the Ansible `tailscale`
    role install the package (it downloads the .deb on the control host) and join the node, which needs
    `admin_ssh_cidr` for the first run.
  EOT
}

variable "headscale_login_server" {
  type        = string
  default     = ""
  description = "Control server URL. Required when bootstrap_tailscale is true."
}

variable "tailscale_authkey" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Pre-auth key (reusable, non-ephemeral). Required when bootstrap_tailscale is true."
}

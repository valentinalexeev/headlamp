variable "name" {
  type        = string
  description = "Node name (VM name prefix); also the tailscale hostname unless `tailscale_hostname` is set."
}

variable "tailscale_hostname" {
  type        = string
  default     = null
  description = "Hostname the node registers under on the tailnet. Defaults to `name`."
}

variable "project_id" {
  type        = string
  description = "Cloud.ru project ID."
}

variable "zone_name" {
  type        = string
  description = "Availability zone name (see the cloudru_evolution_compute_zone_collection data source)."
}

variable "vpc_id" {
  type        = string
  description = "ID of an existing VPC the node's subnet is created in."
}

variable "subnet_address" {
  type        = string
  default     = "10.20.0.0/24"
  description = "CIDR of the subnet this module creates for the node."
}

variable "flavor_name" {
  type        = string
  default     = "gen-1-1"
  description = "Flavor (vCPU/RAM) name; list them with the cloudru_evolution_compute_flavor_collection data source."
}

variable "image_name" {
  type        = string
  default     = "ubuntu-22.04"
  description = "Boot image name, matched exactly against cloudru_evolution_compute_image_collection."
}

variable "image_metadata" {
  type = map(object({
    string_value = optional(string)
    int_value    = optional(number)
    bool_value   = optional(bool)
  }))
  default     = null
  description = "Image metadata some images require (a mandatory group such as authentication). Default: {public_key = ssh_public_key}."
}

variable "disk_type_name" {
  type        = string
  default     = null
  description = "Disk type name; default: the first type returned by cloudru_evolution_compute_disk_type_collection."
}

variable "disk_size_gb" {
  type        = number
  default     = 20
  description = "Boot disk size in GB."
}

variable "ssh_public_key" {
  type        = string
  description = "SSH public key installed for the default user."
}

variable "admin_ssh_cidr" {
  type        = string
  default     = null
  description = "If set, allow SSH (tcp/22) from this CIDR. Default: no public SSH, like the other VM modules (reach the node over the tailnet)."
}

variable "tailscale_port" {
  type        = number
  default     = 41641
  description = "UDP port opened for tailscale's WireGuard traffic."
}

variable "bootstrap_tailscale" {
  type        = bool
  default     = false
  description = <<-EOT
    Install tailscale and join the tailnet from cloud-init on first boot. Off by default because hosts in
    restricted networks often cannot reach tailscale.com / pkgs.tailscale.com reliably: leave it off and let the Ansible
    `tailscale` role install the package (it downloads the .deb on the control host) and join the node, which
    needs `admin_ssh_cidr` for the first run. Turn it on only if the VM can reach those hosts.
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

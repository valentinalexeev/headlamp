variable "name" {
  type        = string
  description = "Node name (instance name); also the tailscale hostname unless `tailscale_hostname` is set."
}

variable "tailscale_hostname" {
  type        = string
  default     = null
  description = "Hostname the node registers under on the tailnet. Defaults to `name`. Set it when the tailnet name differs from the instance name, so a replacement registers correctly without a manual rename."
}

variable "instance_type" {
  type    = string
  default = "t3.micro"
}

variable "ami_arch" {
  type        = string
  default     = "amd64"
  description = "Ubuntu AMI architecture suffix: \"amd64\" or \"arm64\". Must match instance_type's actual CPU architecture (e.g. Graviton c6g/m6g/r6g families need \"arm64\") -- cloud-init's tailscale install.sh auto-detects arch at boot, so no further wiring is needed once these two agree."
}

variable "ubuntu_series" {
  type        = string
  default     = "resolute-26.04"
  description = "Ubuntu series of the Canonical AMI, as \"<codename>-<version>\" in the AMI name. Set \"noble-24.04\" to keep the old image. A change makes a new launch template version. The running instance stays until the group replaces it."

  validation {
    condition     = can(regex("^[a-z]+-[0-9]{2}\\.[0-9]{2}$", var.ubuntu_series))
    error_message = "ubuntu_series: use the form <codename>-<version>, for example \"resolute-26.04\"."
  }
}

variable "image_flavor" {
  type        = string
  default     = "minimal"
  description = "Canonical AMI flavor: \"minimal\" (Ubuntu Minimal, fewer packages and services, no snapd) or \"server\" (the full Ubuntu Server image). A change makes a new launch template version. The running instance stays until the group replaces it."

  validation {
    condition     = contains(["minimal", "server"], var.image_flavor)
    error_message = "image_flavor: use \"minimal\" or \"server\"."
  }
}

variable "ami_id" {
  type        = string
  default     = null
  description = "AMI ID to use instead of the Canonical lookup (ubuntu_series, image_flavor). The image must run cloud-init, for example a Debian 13 AMI of owner 136693071363. Null (default) uses the lookup."
}

variable "spot" {
  type        = bool
  default     = true
  description = "Use Spot pricing. Default true — see design doc Cost section."
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

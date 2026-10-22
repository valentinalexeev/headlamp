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

variable "name" {
  type        = string
  description = "Instance name, also used as the tailscale hostname."
}

variable "folder_id" {
  type        = string
  description = "Yandex Cloud folder ID to create the instance in."
}

variable "zone" {
  type        = string
  description = "Yandex Cloud zone, e.g. <zone>."
}

variable "subnet_id" {
  type        = string
  description = "VPC subnet ID for the instance's network interface."
}

variable "platform_id" {
  type    = string
  default = "standard-v3"
}

variable "cores" {
  type    = number
  default = 2
}

variable "memory" {
  type        = number
  default     = 1
  description = "Memory in GB."
}

variable "core_fraction" {
  type        = number
  default     = 5
  description = "Guaranteed vCPU share percentage; 5 is the cheapest burstable tier."
}

variable "preemptible" {
  type        = bool
  default     = true
  description = "Interruptible/preemptible pricing. Default true — see design doc Cost section."
}

variable "boot_disk_size" {
  type        = number
  default     = 8
  description = "Boot disk size in GB."
}

variable "boot_disk_type" {
  type    = string
  default = "network-hdd"
}

variable "image_id" {
  type        = string
  description = "Boot disk source image ID."
}

variable "ssh_public_key" {
  type        = string
  description = "SSH public key installed for the default admin user."
}

variable "ssh_user" {
  type    = string
  default = "ubuntu"
}

variable "user_data" {
  type        = string
  default     = ""
  description = "Optional cloud-init user-data. Empty by default — existing imported nodes keep their own metadata; only set this for brand-new nodes."
}

variable "extra_metadata" {
  type        = map(string)
  default     = {}
  description = "Additional raw instance metadata key/value pairs, merged in alongside ssh-keys/user-data. Needed when importing an existing instance whose metadata includes keys this module doesn't manage directly (e.g. Yandex's own 'install-unified-agent'/'serial-port-enable' defaults) — set them here so terraform plan doesn't try to delete them."
}

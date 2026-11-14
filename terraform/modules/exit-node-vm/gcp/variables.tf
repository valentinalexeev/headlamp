variable "name" {
  type        = string
  description = "Node name: the instance name, and the tailscale hostname unless `tailscale_hostname` is set. node-control finds the VM by this name."
  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$", var.name))
    error_message = "name must be a GCE instance name: lower case letters, digits and hyphens, starting with a letter, at most 63 characters."
  }
}

variable "tailscale_hostname" {
  type        = string
  default     = null
  description = "Hostname the node registers under on the tailnet. Defaults to `name`."
}

variable "project" {
  type        = string
  default     = null
  description = "Project of the VM. Null (default) uses the provider's project."
}

variable "zone" {
  type        = string
  description = "Zone of the VM, for example \"europe-west3-a\"."
}

variable "machine_type" {
  type        = string
  default     = "e2-micro"
  description = "Machine type of the VM."
}

variable "spot" {
  type        = bool
  default     = true
  description = "Spot VM. A preempted Spot VM is stopped, not deleted (instance_termination_action = STOP), so its disk and its tailnet identity stay. node-control starts it again. False: a standard VM."
}

variable "image" {
  type        = string
  default     = "ubuntu-os-cloud/ubuntu-minimal-2604-lts-amd64"
  description = "Boot image (\"project/family\" or a full image link). The image must run cloud-init (user-data metadata)."
}

variable "disk_size_gb" {
  type        = number
  default     = 10
  description = "Size of the boot disk in GB."
}

variable "network" {
  type        = string
  default     = "default"
  description = "VPC network of the VM."
}

variable "subnetwork" {
  type        = string
  default     = null
  description = "Subnetwork of the VM. Null (default): the network's subnetwork of the zone's region (auto-mode network)."
}

variable "labels" {
  type        = map(string)
  default     = {}
  description = "Labels for the instance and its boot disk (#226). The module adds headlamp-exit-node = \"true\" to the instance only: node-control controls only VMs with this label."
}

variable "service_account_id" {
  type        = string
  default     = null
  description = "Account id (the part before @) of the node's service account. Null (default): \"<name>-exit-node\". The account has no project role; with authkey_from_secret it may read that one secret only."
}

variable "headscale_login_server" {
  type        = string
  description = "URL of the headscale server."
}

variable "tailscale_version" {
  type        = string
  default     = null
  description = "Install this tailscale version (for example \"1.104.5\") from the signed pkgs.tailscale.com apt repository, and hold it. Null (default): the unpinned install.sh."
}

variable "authkey_from_secret" {
  type        = bool
  default     = false
  description = "Read the tailscale auth key at boot from the Secret Manager secret `authkey_secret`, with the node's service account (#223: a plan-time switch, so the secret can be made in the same configuration). The key is then not in the Terraform state, the metadata or the startup log."
}

variable "authkey_secret" {
  type        = string
  default     = null
  description = "The Secret Manager secret of the auth key, as \"projects/<project>/secrets/<name>\". Needed when authkey_from_secret is true. The node reads its latest version."
  validation {
    condition     = var.authkey_secret == null || can(regex("^projects/[^/]+/secrets/[^/]+$", var.authkey_secret))
    error_message = "authkey_secret must be \"projects/<project>/secrets/<name>\"."
  }
}

variable "tailscale_authkey" {
  type        = string
  default     = null
  sensitive   = true
  description = "The tailscale auth key in the metadata. It is then in the Terraform state and the instance metadata. Prefer authkey_from_secret. Set either this or authkey_from_secret."
}

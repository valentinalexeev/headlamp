variable "name" {
  type        = string
  description = "Name prefix for every resource, for example \"headlamp-control\". Lower case letters, digits and -."
  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,40}[a-z0-9])?$", var.name))
    error_message = "name must be 1 to 42 lower case letters, digits or -, start with a letter and not end with -."
  }
}

variable "project" {
  type        = string
  description = "Project of the host."
}

variable "region" {
  type        = string
  description = "Region of the static address and the load balancer, for example \"europe-west3\". It must contain zone."
}

variable "zone" {
  type        = string
  description = "Zone of the VM and its data disk, for example \"europe-west3-a\"."
}

variable "machine_type" {
  type        = string
  default     = "e2-small"
  description = "Machine type. headscale, node-control and funnel-ingress fit in 2 GiB."
}

variable "image" {
  type        = string
  default     = "ubuntu-os-cloud/ubuntu-minimal-2604-lts-amd64"
  description = "Boot image (\"project/family\" or a full image link). It must run cloud-init (user-data metadata). The VM ignores image changes, so a change applies only to a new VM."
}

variable "boot_disk_gb" {
  type    = number
  default = 20
}

variable "data_disk_gb" {
  type        = number
  default     = 10
  description = "Size of the persistent data disk mounted at /srv/headlamp/data. It holds the headscale database and the state of the apps."
}

variable "data_disk_type" {
  type    = string
  default = "pd-balanced"
}

variable "data_snapshot" {
  type        = string
  default     = null
  description = "Optional snapshot to create the data disk from, for a restore. It is read only when the disk is created."
}

variable "snapshot_retain_days" {
  type        = number
  default     = 14
  description = "Daily snapshots of the data disk are kept this many days. 0 turns snapshots off."
  validation {
    condition     = var.snapshot_retain_days >= 0 && floor(var.snapshot_retain_days) == var.snapshot_retain_days
    error_message = "snapshot_retain_days must be a whole number, 0 or more."
  }
}

variable "network" {
  type        = string
  default     = "default"
  description = "VPC network of the host."
}

variable "subnetwork" {
  type        = string
  default     = null
  description = "Optional subnetwork, in region. Default: the network's subnetwork of the region (auto mode networks)."
}

variable "load_balancer" {
  type        = bool
  default     = false
  description = "false (default): a static external address on the VM. true: an external passthrough network load balancer sends 443/tcp, 80/tcp and 3478/udp to the VM, and keeps the client address. TLS ends on the host either way."
}

variable "admin_cidrs" {
  type        = list(string)
  default     = []
  description = "Optional CIDRs allowed to SSH (22/tcp). Default: none. Use IAP (35.235.240.0/20) or the tailnet; access is through OS Login."
}

variable "dns_zone" {
  type        = string
  default     = null
  description = "Optional Cloud DNS managed zone name (in dns_zone_project) that the host may change: ACME DNS-01 for funnel wildcards, and the funnel record of node-control (FUNNEL_DNS_PROVIDER=googleclouddns). The host's service account gets roles/dns.admin on this zone only."
}

variable "dns_zone_project" {
  type        = string
  default     = null
  description = "Project of dns_zone. Default: project."
}

variable "secrets" {
  type        = list(string)
  default     = []
  description = "Optional Secret Manager secrets (\"projects/<p>/secrets/<s>\") that the host may read, for gcpsm:// references in the node-control environment. Each gets roles/secretmanager.secretAccessor for the host's service account."
  validation {
    condition     = alltrue([for s in var.secrets : can(regex("^projects/[^/]+/secrets/[^/]+$", s))])
    error_message = "each secret must be \"projects/<project>/secrets/<name>\"."
  }
}

variable "user_data_extra" {
  type        = string
  default     = ""
  description = "Optional shell commands that cloud-init runs after Docker is installed and the data disk is mounted, for example to fetch your compose files."
}

variable "labels" {
  type    = map(string)
  default = {}
}

variable "name" {
  type        = string
  description = "Name prefix for every resource, for example \"headlamp-control\"."
}

variable "vpc_id" {
  type        = string
  default     = null
  description = "VPC for the host. Default: the default VPC of the region."
}

variable "subnet_id" {
  type        = string
  default     = null
  description = "Public subnet for the host. Default: the first subnet of the VPC. The data volume is created in the same availability zone."
}

variable "instance_type" {
  type        = string
  default     = "t3.small"
  description = "Instance type. headscale, node-control and funnel-ingress fit in 2 GiB. Match ami_arch to it."
}

variable "ami_arch" {
  type        = string
  default     = "amd64"
  description = "Ubuntu AMI architecture: \"amd64\" or \"arm64\" (Graviton). It must match instance_type. The published images are linux/amd64 only, so arm64 needs images you build yourself."
  validation {
    condition     = contains(["arm64", "amd64"], var.ami_arch)
    error_message = "ami_arch must be \"arm64\" or \"amd64\"."
  }
}

variable "root_volume_gb" {
  type    = number
  default = 20
}

variable "data_volume_gb" {
  type        = number
  default     = 10
  description = "Size of the EBS data volume mounted at /srv/headlamp/data. It holds the headscale database and the state of the apps."
}

variable "snapshot_retain_days" {
  type        = number
  default     = 14
  description = "Daily snapshots of the data volume are kept this many days. 0 turns snapshots off."
}

variable "data_snapshot_id" {
  type        = string
  default     = null
  description = "Optional snapshot to create the data volume from, for a restore. It is read only when the volume is created."
}

variable "load_balancer" {
  type        = bool
  default     = true
  description = "true: a network load balancer passes TCP 443 and 80 and UDP 3478 to the host, and TLS ends on the host. false: an Elastic IP on the host, with the same ports open directly."
}

variable "admin_cidrs" {
  type        = list(string)
  default     = []
  description = "Optional CIDRs allowed to SSH (22/tcp). Default: none. Use SSM Session Manager or the tailnet instead."
}

variable "ssh_public_key" {
  type        = string
  default     = null
  description = "Optional SSH public key. Without it the host has no key pair, and you use SSM Session Manager."
}

variable "route53_zone_id" {
  type        = string
  default     = null
  description = "Optional hosted zone that the host may change, for ACME DNS-01 (wildcard certificates for funnel-ingress). The instance role gets change rights on this zone only."
}

variable "user_data_extra" {
  type        = string
  default     = ""
  description = "Optional shell commands that cloud-init runs after Docker is installed and the data volume is mounted, for example to fetch your compose files."
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "iam_path" {
  type        = string
  default     = "/"
  description = "The IAM path of the host role, the snapshot role and the instance profile. Set it when the deploying identity may create roles only under a fixed path."
  validation {
    condition     = can(regex("^/([\\x21-\\x7e]+/)*$", var.iam_path)) && length(var.iam_path) <= 512
    error_message = "iam_path must start and end with /, for example /headlamp/."
  }
}

variable "iam_permissions_boundary" {
  type        = string
  default     = null
  description = "Optional ARN of a managed policy that is set as the permissions boundary of both roles. Set it when the account requires a boundary on new roles."
  validation {
    condition     = var.iam_permissions_boundary == null || can(regex("^arn:aws[a-z-]*:iam::(aws|[0-9]{12}):policy/.+$", var.iam_permissions_boundary))
    error_message = "iam_permissions_boundary must be an IAM policy ARN, for example arn:aws:iam::123456789012:policy/name."
  }
}

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
  type        = string
  default     = null
  description = "Optional public key for an EC2 key pair. Null (default): no key pair. The security group has no SSH, and admin access is over the tailnet."
}

variable "headscale_login_server" {
  type = string
}

variable "tailscale_authkey" {
  type        = string
  default     = null
  sensitive   = true
  description = "The tailscale auth key, put into the user data. It is then in the Terraform state and in every launch template version. Prefer authkey_secret_arn. Set either this or authkey_secret_arn."
}

variable "authkey_secret_arn" {
  type        = string
  default     = null
  description = "ARN of a Secrets Manager secret that holds the tailscale auth key as a plain SecretString. When set, the node reads the key at boot with its instance role. The key is then not in the Terraform state, the user data or the launch template. Set either this or tailscale_authkey."

  validation {
    condition     = var.authkey_secret_arn == null || can(regex("^arn:aws[a-z-]*:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:.+$", var.authkey_secret_arn))
    error_message = "authkey_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "authkey_kms_key_arn" {
  type        = string
  default     = null
  description = "ARN of the customer managed KMS key of the secret, if it is not the AWS managed key. The role then gets kms:Decrypt on this key only."
}

variable "enable_ssm" {
  type        = bool
  default     = false
  description = "Attach AmazonSSMManagedInstanceCore to the node role, for SSM Session Manager."
}

variable "iam_path" {
  type        = string
  default     = "/"
  description = "The IAM path of the node role and the instance profile. Set it when the deploying identity may create roles only under a fixed path."

  validation {
    condition     = can(regex("^/([\\x21-\\x7e]+/)*$", var.iam_path)) && length(var.iam_path) <= 512
    error_message = "iam_path must start and end with /, for example /headlamp/."
  }
}

variable "iam_permissions_boundary" {
  type        = string
  default     = null
  description = "Optional ARN of a managed policy that is set as the permissions boundary of the node role."

  validation {
    condition     = var.iam_permissions_boundary == null || can(regex("^arn:aws[a-z-]*:iam::(aws|[0-9]{12}):policy/.+$", var.iam_permissions_boundary))
    error_message = "iam_permissions_boundary must be an IAM policy ARN, for example arn:aws:iam::123456789012:policy/name."
  }
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Tags for every resource. The group also puts them on each instance and its volumes at launch."
}

variable "tailscale_version" {
  type        = string
  default     = null
  description = "Install this tailscale version (for example \"1.104.5\") from the signed pkgs.tailscale.com apt repository, and hold it. Null (default) keeps the old unpinned install.sh. The repository must have the image's distribution and codename."

  validation {
    condition     = var.tailscale_version == null || can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+$", var.tailscale_version))
    error_message = "tailscale_version must be a version like 1.104.5."
  }
}

variable "vpc_id" {
  type        = string
  default     = null
  description = "VPC of the node. Null (default) uses the default VPC. Set subnet_ids together with it."
}

variable "subnet_ids" {
  type        = list(string)
  default     = null
  description = "Subnets of the group. Null (default) uses all subnets of the VPC. The subnets must give a public IPv4 address or have a NAT route."
}

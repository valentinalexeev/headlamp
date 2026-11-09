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
  default     = "Ubuntu 26.04 LTS 64-bit"
  description = "Public image name, matched exactly (most recent). Set \"Ubuntu 22.04 LTS 64-bit\" to keep the old image for a new server, or \"Debian 13 (Trixie) 64-bit\" (Selectel) for Debian 13. image_id overrides it."
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

variable "external_network_name" {
  type        = string
  default     = "external-network"
  description = <<-EOT
    Name of the external (public) network: the router's gateway and the floating IP pool. The default is the name
    Selectel uses. Another OpenStack-compatible cloud usually names it differently (often "public" or "ext-net");
    `openstack network list --external` shows it.
  EOT
}

variable "image_id" {
  type        = string
  default     = null
  description = "Image ID to boot from. It pins the image (#235): a rebuild uses the same image. Null (default): the newest public image named image_name."
}

variable "advertise_exit_node" {
  type        = bool
  default     = true
  description = "With bootstrap_tailscale, run `tailscale up --advertise-exit-node`. Set false for a bridge (reality-ingress, #235): a bridge must not advertise exit routes."
}

variable "extra_tcp_ingress_ports" {
  type        = list(number)
  default     = []
  description = "Extra TCP ports open inbound from anywhere, for a node that also serves a public front. A bridge needs [80, 443]: ACME HTTP-01 and Reality. Empty (default): only the tailscale UDP port."

  validation {
    condition     = alltrue([for p in var.extra_tcp_ingress_ports : p >= 1 && p <= 65535 && p == floor(p) && p != 22])
    error_message = "extra_tcp_ingress_ports: whole numbers 1-65535, and never 22 (SSH is admin_ssh_cidr only)."
  }
}

variable "extra_udp_ingress_ports" {
  type        = list(number)
  default     = []
  description = "Extra UDP ports open inbound from anywhere. A bridge with Hysteria 2 needs [443] (hy2_listen_port). Empty (default): none."

  validation {
    condition     = alltrue([for p in var.extra_udp_ingress_ports : p >= 1 && p <= 65535 && p == floor(p)])
    error_message = "extra_udp_ingress_ports: whole numbers 1-65535."
  }
}

variable "tags" {
  type        = list(string)
  default     = []
  description = "Tags on the instance, port, network, subnet, router, security group and floating IP (Nova and Neutron tags). Use them to find the node's resources, e.g. openstack port list --tags <tag>. The instance also gets \"preemptible\" when preemptible = true."
}

variable "metadata" {
  type        = map(string)
  default     = {}
  description = "Metadata (key = value) on the instance and the boot volume. Cinder volumes have no tags, so use this to find the volume, e.g. openstack volume list --property <key>=<value>."
}

variable "authkey_from_secret" {
  type        = bool
  default     = false
  description = <<-EOT
    true: the node reads the auth key at boot from the Barbican secret authkey_secret_ref (#224), not from user data.
    Terraform creates an application credential that can only GET that secret's payload, and puts it in user data.
    A plan-time bool, so that a secret made in the same apply does not break the plan (#222).
  EOT
}

variable "authkey_secret_ref" {
  type        = string
  default     = null
  description = "Barbican secret href, for example https://<key-manager>/v1/secrets/<uuid>. Its payload is the auth key. Required when authkey_from_secret = true."

  validation {
    condition     = var.authkey_secret_ref == null || can(regex("^https://[^/]+/.*secrets/[0-9a-fA-F-]{36}$", var.authkey_secret_ref))
    error_message = "authkey_secret_ref must be a Barbican secret href: https://<host>/.../secrets/<uuid>."
  }
}

variable "keystone_auth_url" {
  type        = string
  default     = null
  description = "Keystone v3 URL that the node uses to get a token, for example https://<keystone>/v3. Required when authkey_from_secret = true."
}

variable "authkey_credential_expires_at" {
  type        = string
  default     = null
  description = "RFC 3339 time when the node's application credential stops working, for example 2027-01-01T00:00:00Z. Null: it does not expire. The node needs it only on the first boot."
}

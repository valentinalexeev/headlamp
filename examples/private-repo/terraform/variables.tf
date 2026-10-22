# No defaults for account identifiers: they belong in your private terraform.tfvars / sops files.
variable "oci_region" { type = string }
variable "oci_compartment_id" { type = string }
variable "yandex_cloud_id" { type = string }
variable "yandex_folder_id" { type = string }
variable "yandex_zone" { type = string }
variable "yandex_service_account_key_file" {
  type        = string
  description = "Path to the decrypted service-account key (tf-run.sh decrypts yandex-sa-terraform-key.json.enc.json)."
}
variable "aws_region" { type = string }

variable "ssh_public_key" {
  type        = string
  description = "SSH public key installed on all provisioned VMs."
}
variable "node1_tailscale_authkey" {
  type      = string
  sensitive = true
}
variable "headscale_login_server" { type = string }

# Test fixture (#222): a root stack that creates the auth key secret in the SAME apply as the node. The ARN is then
# unknown at plan time, so the module must not decide a count from it.
terraform {
  required_providers {
    aws = { source = "hashicorp/aws" }
  }
}


variable "authkey_from_secret" {
  type    = bool
  default = true
}

resource "aws_secretsmanager_secret" "authkey" {
  name = "headlamp/n1/authkey"
}

module "node" {
  source                 = "../.."
  name                   = "n1"
  headscale_login_server = "https://hs.example.com"
  authkey_secret_arn     = aws_secretsmanager_secret.authkey.arn
  authkey_from_secret    = var.authkey_from_secret
  tailscale_version      = "1.104.5"
}

output "role_policies" {
  value = module.node.role_name
}

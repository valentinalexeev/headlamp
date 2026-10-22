terraform {
  required_version = ">= 1.5"
  required_providers {
    cloudru = {
      # Official Cloud.ru Evolution provider (the vendor's open-source repository). Not on the public
      # Terraform registry yet: install it through Cloud.ru's network mirror or as a local plugin, see
      # https://cloud.ru/docs/terraform-evolution/ug/topics/quickstart. Its schema changes between minor
      # versions (the external IP arguments did), so pin the exact line this module was written against.
      source  = "cloudru/cloud"
      version = "~> 2.1.5"
    }
  }
}

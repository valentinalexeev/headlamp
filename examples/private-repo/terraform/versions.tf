terraform {
  required_version = ">= 1.5"
  required_providers {
    oci    = { source = "oracle/oci", version = "~> 6.0" }
    yandex = { source = "yandex-cloud/yandex", version = "~> 0.130" }
    aws    = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

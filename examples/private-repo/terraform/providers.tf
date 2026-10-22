# Remove the providers you don't use (and their entries in versions.tf).
# Credentials come from dedicated CLI profiles scoped to a narrow compartment/folder/account,
# never your everyday identity, and never from this repo.
provider "oci" {
  auth                = "ApiKey"
  config_file_profile = "headlamp" # ~/.oci/config [headlamp]
  region              = var.oci_region
}

provider "yandex" {
  cloud_id                 = var.yandex_cloud_id
  folder_id                = var.yandex_folder_id
  service_account_key_file = var.yandex_service_account_key_file # decrypted transiently by tf-run.sh
  zone                     = var.yandex_zone
}

provider "aws" {
  region  = var.aws_region
  profile = "headlamp" # ~/.aws/credentials [headlamp]
}

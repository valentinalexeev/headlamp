terraform {
  required_version = ">= 1.5"
  required_providers {
    # This module only needs the openstack provider, configured in your root stack against the Keystone of your cloud
    # (Selectel or another OpenStack cloud, see README.md). Projects and service users are created outside this module.
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.4"
    }
  }
}

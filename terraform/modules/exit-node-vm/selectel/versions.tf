terraform {
  required_version = ">= 1.5"
  required_providers {
    # Selectel cloud servers are plain OpenStack: this module only needs the openstack provider, configured in
    # your root stack against Selectel's Keystone (see README.md). Projects and service users are created with
    # the selectel provider (or the control panel), outside this module.
    openstack = {
      source  = "terraform-provider-openstack/openstack"
      version = "~> 3.4"
    }
  }
}

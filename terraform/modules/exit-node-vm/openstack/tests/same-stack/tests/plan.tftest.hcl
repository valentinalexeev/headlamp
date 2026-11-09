# The #222 case for OpenStack (#224): the secret href is unknown at plan time, and the plan must still work.
#   cd terraform/modules/exit-node-vm/openstack/tests/same-stack && terraform init -backend=false && terraform test
mock_provider "openstack" {
  mock_data "openstack_images_image_v2" {
    defaults = { id = "img-ubuntu" }
  }
  mock_data "openstack_networking_network_v2" {
    defaults = { id = "net-ext", name = "external-network" }
  }
}

run "same_stack_secret_plans" {
  command = plan

  assert {
    condition     = length(module.node.application_credential_id) == 1
    error_message = "a secret made in the same apply must plan, with the application credential"
  }
}

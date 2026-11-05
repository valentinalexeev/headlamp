# exit-node-vm/selectel

This is the old name of [`exit-node-vm/openstack`](../openstack/README.md). It calls that module with the same inputs
and outputs.

**Deprecated.** This module will be removed by 1.0.0. Move your root stack to `exit-node-vm/openstack`.

Existing root stacks continue to work. The `moved` blocks in `main.tf` move the state into the new module, so the
first plan after the upgrade shows no change. For a new root stack, use `exit-node-vm/openstack`.

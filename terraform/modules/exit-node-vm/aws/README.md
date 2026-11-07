# exit-node-vm/aws

This module makes one exit node on AWS EC2. An auto scaling group with a size of one runs the node. The node joins your
tailnet with the auth key that you give.

## Boot image

The module uses the newest Canonical AMI of the Ubuntu series in `ubuntu_series`. The default is `resolute-26.04`
(Ubuntu 26.04 LTS).

A change of the series makes a new version of the launch template. The running instance stays. The group uses the new
image only when it starts a new instance, for example after a stop and start by node-control.

To keep the old image on all new instances, set the series in your root stack:

```hcl
module "node" {
  source        = "../vendor/headlamp/terraform/modules/exit-node-vm/aws"
  ubuntu_series = "noble-24.04"
  # the other inputs as usual
}
```

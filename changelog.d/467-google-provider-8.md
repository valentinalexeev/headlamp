---
issue: 467
type: task
section: Changed
status: done
breaking: true
title: The GCP modules need the Google provider 8
components: [terraform]
pr: 0
---
## Summary

The Terraform modules `exit-node-vm/gcp` (#462) and `control-host/gcp` now need the Google provider `~> 8.11`
instead of `~> 7.0`. The plan tests of both modules pass with provider 8.12.0. The resources and the inputs of the
modules do not change.

## Upgrade

If your site configuration pins `hashicorp/google` to 7.x, change the pin to allow 8.11 or later. Then run
`terraform init -upgrade` and check that `terraform plan` shows no changes before you apply.

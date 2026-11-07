# exit-node-vm/oci

This module makes one exit node on Oracle Cloud Infrastructure (OCI). The node joins your tailnet with the auth key
that you give.

## Boot image

The module uses the newest "Canonical Ubuntu" platform image of the release in `ubuntu_version`. The default is
`24.04`.

The default stays at `24.04` because OCI has no Ubuntu 26.04 platform image yet. When OCI publishes one, set
`ubuntu_version = "26.04"` to use it.

Terraform ignores a change of the image after the instance exists. Thus a change of `ubuntu_version` does not replace
a running node. To make a new node with the new image, run
`terraform apply -replace='module.<name>.oci_core_instance.this'`.

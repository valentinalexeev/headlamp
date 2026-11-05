#!/usr/bin/env bash
# Checks that the exit-node-vm/selectel wrapper (#101) keeps existing state: a root stack that applied the old
# selectel module plans no create or destroy after it switches to the wrapper, because of the moved blocks.
# Uses Terraform's mock provider (terraform test), so no cloud account is needed. Needs terraform >= 1.7 and network
# access to the provider registry. usage: run_tests.sh BASE_REF   (the commit that still has the old module)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"; root="$here/../../.."
base=${1:?usage: run_tests.sh BASE_REF}
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/old/templates" "$tmp/stack/tests"
for f in main.tf outputs.tf variables.tf versions.tf templates/cloud-init.yaml.tmpl; do
	git -C "$root" show "$base:terraform/modules/exit-node-vm/selectel/$f" >"$tmp/old/$f"
done
cp -R "$root/terraform/modules/exit-node-vm/openstack" "$tmp/openstack"
cp -R "$root/terraform/modules/exit-node-vm/selectel" "$tmp/selectel"

stack() { # $1 = module source
	cat <<EOF
terraform {
  required_providers {
    openstack = { source = "terraform-provider-openstack/openstack", version = "~> 3.4" }
  }
}
module "node" {
  source            = "$1"
  name              = "node-b"
  availability_zone = "zone-a"
  flavor_id         = "1011"
  volume_type       = "fast.zone-a"
  ssh_public_key    = "ssh-ed25519 AAAAexample example"
}
EOF
}
mock='mock_provider "openstack" {
  mock_data "openstack_images_image_v2" { defaults = { id = "img-1" } }
  mock_data "openstack_networking_network_v2" { defaults = { id = "net-ext", name = "external-network" } }
}'

# 1. Apply the old layout into a local state file.
stack ../old >"$tmp/stack/main.tf"
printf '%s\nrun "apply_old" {\n  command = apply\n}\n' "$mock" >"$tmp/stack/tests/a.tftest.hcl"
(cd "$tmp/stack" && terraform init -input=false -no-color >/dev/null && terraform test -no-color >/dev/null)

# terraform test keeps no state, so the second step plans the wrapper and checks the moved blocks cover every
# resource of the old module: each old address must be the "from" of exactly one moved block.
old=$(grep -E '^resource ' "$tmp/old/main.tf" | sed -E 's/^resource "([^"]+)" "([^"]+)".*/\1.\2/' | sort)
moved=$(grep -A1 '^moved' "$tmp/selectel/main.tf" | sed -n 's/^ *from *= *//p' | sort)
if [ "$old" != "$moved" ]; then
	echo "FAIL - moved blocks do not match the old resources"; diff <(echo "$old") <(echo "$moved") || true; exit 1
fi
echo "ok   - every old resource has a moved block ($(echo "$old" | wc -l | tr -d ' '))"

# 2. The wrapper plans with the same inputs, and its outputs keep their names.
stack ../selectel >"$tmp/stack/main.tf"
printf '%s\nrun "plan_wrapper" {\n  command = plan\n}\n' "$mock" >"$tmp/stack/tests/a.tftest.hcl"
(cd "$tmp/stack" && terraform init -input=false -no-color >/dev/null && terraform test -no-color >/dev/null)
diff <(sed -n 's/^output "\(.*\)".*/\1/p' "$tmp/old/outputs.tf" | sort) <(sed -n 's/^output "\(.*\)".*/\1/p' "$tmp/selectel/outputs.tf" | sort) >/dev/null ||
	{ echo "FAIL - outputs changed"; exit 1; }
diff <(sed -n 's/^variable "\(.*\)".*/\1/p' "$tmp/old/variables.tf" | sort) <(sed -n 's/^variable "\(.*\)".*/\1/p' "$tmp/selectel/variables.tf" | grep -v '^external_network_name$' | sort) >/dev/null ||
	{ echo "FAIL - inputs changed"; exit 1; }
echo "ok   - the wrapper plans with the old inputs and keeps every input and output"

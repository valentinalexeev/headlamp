# exit-node-vm/aws

This module makes one exit node on AWS EC2. An auto scaling group with a size of one runs the node. The node joins your
tailnet with the auth key that you give.

## Auth key

With node-control's stop and start (ADR 0001), each start makes a new instance, and it joins again. So the auth key must
be reusable and must live long. Keep it out of the Terraform state:

1. Put the key in Secrets Manager as a plain `SecretString`.
2. Set `authkey_secret_arn` to the ARN of the secret. Do not set `tailscale_authkey`.

The module then makes a node role that can read only that secret (`secretsmanager:GetSecretValue`). At boot, the node
reads the key with IMDSv2 and the role, and gives it to `tailscale up` as a file. The key is not in the state, the user
data or the launch template. Check it:

```sh
aws ec2 describe-launch-template-versions --launch-template-id <id> \
  --query 'LaunchTemplateVersions[0].LaunchTemplateData.UserData' --output text | base64 -d | grep -c tskey   # 0
```

If the secret uses a customer managed KMS key, also set `authkey_kms_key_arn`.

`tailscale_authkey` still works. It puts the key into the user data, so it is in the state and in every launch template
version. Set exactly one of the two.

## Inputs

| Name | Default | What it does |
|---|---|---|
| `name` | required | The instance name and the tailnet hostname |
| `headscale_login_server` | required | The headscale URL |
| `authkey_secret_arn` | none | The secret with the auth key. Recommended. |
| `tailscale_authkey` | none | The auth key in the user data. Use only if you cannot use a secret. |
| `authkey_kms_key_arn` | none | The KMS key of the secret, if it is a customer managed key |
| `ssh_public_key` | none | An optional key pair. Admin access is over the tailnet. |
| `tailscale_version` | none | Install this version from the signed apt repository and hold it. None: the unpinned `install.sh`. |
| `enable_ssm` | `false` | Attach `AmazonSSMManagedInstanceCore` to the node role |
| `iam_path`, `iam_permissions_boundary` | `/`, none | The IAM path and the permissions boundary of the node role |
| `tags` | none | Tags for every resource. The group puts them on each instance and its volumes. |
| `vpc_id`, `subnet_ids` | the default VPC, all its subnets | Where the node runs |
| `tailscale_hostname` | `name` | The tailnet hostname, if it is not `name` |
| `instance_type`, `ami_arch`, `spot` | `t3.micro`, `amd64`, `true` | The instance |
| `ubuntu_series`, `image_flavor`, `ami_id` | see "Boot image" | The image |

The node gets a role only with `authkey_secret_arn` or `enable_ssm`. IMDSv2 is required, with hop limit 1.

### Existing nodes

A caller that sets `tailscale_authkey` and `ssh_public_key` gets the same user data as before. The plan shows only the
new IMDSv2 metadata options, which make a new launch template version. The running instance stays. A new instance gets
the new version at the next start. To move to a secret, set `authkey_secret_arn` and remove `tailscale_authkey`.

Provider `default_tags` do not reach the tags of the auto scaling group. Set `tags` so that each instance gets them at
launch. Accounts that limit creates by request tag need this.

## Boot image

The module uses the newest Canonical AMI of the Ubuntu series in `ubuntu_series` and the flavor in `image_flavor`. The
default series is `resolute-26.04` (Ubuntu 26.04 LTS). The default flavor is `minimal` (Ubuntu Minimal). To use the
full Ubuntu Server image, set `image_flavor = "server"`. To use a different AMI, set `ami_id`. The AMI must run
cloud-init.

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

### Supported images

The table gives the default image and the override for each provider of `exit-node-vm`. All images must run
cloud-init. The Ansible roles are tested on Ubuntu only.

| Provider | Default image | Override |
|---|---|---|
| `aws` | Ubuntu 26.04 Minimal (Canonical AMI) | `image_flavor = "server"` for Ubuntu Server. `ami_id` for a different AMI, for example Debian 13 |
| `oci` | Ubuntu 26.04 Minimal. If OCI has no such image, Ubuntu 24.04 Minimal | `ubuntu_version`, `ubuntu_version_fallback` |
| `yandex` | Ubuntu 26.04 LTS (family `ubuntu-2604-lts`) | `image_family` or `image_id`, for example family `debian-13` |
| `openstack`, `selectel` | `Ubuntu 26.04 LTS 64-bit` | `image_name` |
| `cloudru` | `ubuntu-22.04` | `image_name` |

Ubuntu Minimal has fewer packages and services than Ubuntu Server, and it has no snapd. Yandex Cloud, Selectel and
Cloud.ru do not publish a Minimal image, so these modules use the server image.

# exit-node-vm/aws

This module makes one exit node on AWS EC2. An auto scaling group with a size of one runs the node. The node joins your
tailnet with the auth key that you give.

## Node identity across stop and start

node-control stops a node by scaling its group to 0, so each start makes a **new instance** (ADR 0001). With the
default, tailscaled keeps its state on the instance disk. So the new instance has a new machine key. Headscale then
registers a new node: a new id, a new IP and a name such as `cobalt-1`. The old node stays as an offline record.

To keep the same node, set `state_parameter_name`:

```hcl
module "cobalt" {
  source               = "../vendor/headlamp/terraform/modules/exit-node-vm/aws"
  name                 = "cobalt"
  state_parameter_name = "/headlamp/cobalt/tailscaled"
  tailscale_version    = "1.104.5"
  # the other inputs as usual
}
```

Then tailscaled keeps its state in SSM Parameter Store (a `SecureString`), not on the disk. A new instance loads the
same machine key and is the same node, with the same id, name and IPs. It does not use the auth key again.

- The node role gets `ssm:GetParameter` and `ssm:PutParameter` on that one parameter. With `state_kms_key_arn`, it
  also gets `kms:Encrypt` and `kms:Decrypt` on that key, through SSM only. With no key, SSM uses `aws/ssm`.
- tailscaled makes the parameter at the first start. `terraform destroy` does not delete it. Delete it by hand when
  you remove the node: `aws ssm delete-parameter --name /headlamp/cobalt/tailscaled`.
- Use one parameter for one node. Two running instances with the same parameter are one node with two connections.
  The group has a maximum size of 1, so this can occur only for a short time during a Spot replacement.
- The state holds the node's private keys. Anyone who can read the parameter can act as the node.

With an existing node, the first start with the new setting registers once more, because the state is not in SSM
yet. Delete the old node in headscale after that start.

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
| `authkey_from_secret` | from `authkey_secret_arn` | Set `true` when the secret is made in the same stack |
| `authkey_kms_key_arn` | none | The KMS key of the secret, if it is a customer managed key |
| `ssh_public_key` | none | An optional key pair. Admin access is over the tailnet. |
| `tailscale_version` | none | Install this version from the signed apt repository and hold it. None: the unpinned `install.sh`. |
| `state_parameter_name` | none | Keep the tailscaled state in this SSM parameter, so a stop and start keeps the same node |
| `state_kms_key_arn` | none | The KMS key of the state parameter, if it is a customer managed key |
| `enable_ssm` | `false` | Attach `AmazonSSMManagedInstanceCore` to the node role |
| `iam_path`, `iam_permissions_boundary` | `/`, none | The IAM path and the permissions boundary of the node role |
| `tags` | none | Tags for every resource. The group puts them on each instance and its volumes. |
| `vpc_id`, `subnet_ids` | the default VPC, all its subnets | Where the node runs |
| `tailscale_hostname` | `name` | The tailnet hostname, if it is not `name` |
| `instance_type`, `ami_arch`, `spot` | `t3.micro`, `amd64`, `true` | The instance |
| `ubuntu_series`, `image_flavor`, `ami_id` | see "Boot image" | The image |

The node gets a role only with `authkey_secret_arn`, `state_parameter_name` or `enable_ssm`. IMDSv2 is required, with hop limit 1.

### Existing nodes

A caller that sets `tailscale_authkey` and `ssh_public_key` gets the same user data as before. The plan shows only the
new IMDSv2 metadata options, which make a new launch template version. The running instance stays. A new instance gets
the new version at the next start. To move to a secret, set `authkey_secret_arn` and remove `tailscale_authkey`.

Provider `default_tags` do not reach the tags of the auto scaling group. Set `tags` so that each instance gets them at
launch. Accounts that limit creates by request tag need this.

With `tags`, the launch template tags the instance, its volume and its network interface in the `RunInstances` request
(#225). So a deployer that may create these only with the site tags can launch.

### Secret made in the same stack

If the root stack makes the secret in the same apply, its ARN is unknown at plan time. Then set
`authkey_from_secret = true` (#222), else the plan fails with "Invalid count argument":

```hcl
resource "aws_secretsmanager_secret" "authkey" {
  name = "headlamp/cobalt/authkey"
}

module "cobalt" {
  source              = "../vendor/headlamp/terraform/modules/exit-node-vm/aws"
  authkey_secret_arn  = aws_secretsmanager_secret.authkey.arn
  authkey_from_secret = true
  # the other inputs as usual
}
```

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
cloud-init. The Ansible roles are tested on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13 (#178).

| Provider | Default image | Override |
|---|---|---|
| `aws` | Ubuntu 26.04 Minimal (Canonical AMI) | `image_flavor = "server"` for Ubuntu Server. `ami_id` for a different AMI, for example Debian 13 |
| `oci` | Ubuntu 26.04 Minimal. If OCI has no such image, Ubuntu 24.04 Minimal | `ubuntu_version`, `ubuntu_version_fallback` |
| `yandex` | Ubuntu 26.04 LTS (family `ubuntu-2604-lts`) | `image_family` or `image_id`, for example family `debian-13` |
| `openstack`, `selectel` | `Ubuntu 26.04 LTS 64-bit` | `image_name`, or `image_id` to pin one image. Selectel: `Debian 13 (Trixie) 64-bit` (#235) |
| `cloudru` | `ubuntu-22.04` | `image_name` |

Ubuntu Minimal has fewer packages and services than Ubuntu Server, and it has no snapd. Yandex Cloud, Selectel and
Cloud.ru do not publish a Minimal image, so these modules use the server image.

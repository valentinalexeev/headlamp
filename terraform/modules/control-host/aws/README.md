# control-host/aws

This module makes one EC2 host for the central nodes of headlamp: `headscale`, `node-control` and `funnel-ingress`.
They run with Docker Compose, from [`examples/compose/control`](../../../../examples/compose/control/). That README
gives the full procedure.

## What it makes

- One EC2 instance: Ubuntu 26.04 LTS, Docker and the compose plugin, IMDSv2 only.
- An encrypted gp3 data volume, mounted at `/srv/headlamp/data`. The module formats it only when it is empty, so a new
  instance keeps the tailnet. The volume has `prevent_destroy`.
- Daily snapshots of the data volume (`snapshot_retain_days`, default 14, `0` turns them off).
- A network load balancer that passes 443/tcp, 80/tcp and 3478/udp to the instance. It does not end TLS, and it keeps
  the client address. With `load_balancer = false` the instance gets an Elastic IP.
- A security group with these three ports. SSH is closed unless you set `admin_cidrs`.
- An instance role for SSM Session Manager. With `route53_zone_id`, the role can also change that one zone, for
  ACME DNS-01.

## Inputs

| Name | Default | What it does |
|---|---|---|
| `name` | required | The name prefix of all resources |
| `vpc_id`, `subnet_id` | the default VPC, its first subnet | Where the host runs. The subnet must be public. |
| `instance_type`, `ami_arch` | `t3.small`, `amd64` | The published images are `linux/amd64` only. |
| `root_volume_gb`, `data_volume_gb` | `20`, `10` | Disk sizes |
| `snapshot_retain_days` | `14` | The number of daily snapshots to keep |
| `data_snapshot_id` | none | Make the data volume from this snapshot, for a restore |
| `load_balancer` | `true` | A load balancer, or an Elastic IP |
| `admin_cidrs` | none | Addresses that can use SSH |
| `ssh_public_key` | none | An optional key pair |
| `route53_zone_id` | none | The zone for ACME DNS-01 |
| `iam_path` | `/` | The IAM path of both roles and the instance profile |
| `iam_permissions_boundary` | none | The ARN of a policy to set as the permissions boundary of both roles |
| `user_data_extra` | none | Shell commands that cloud-init runs at the end |
| `tags` | none | Tags for all resources |

Some accounts let the deploying identity create roles only under a fixed IAM path, and only with a permissions boundary. In such an account, set `iam_path` and `iam_permissions_boundary`. With the defaults, the plan of an existing host shows no change.

## Outputs

`public_address` (point your DNS names here), `load_balancer_zone_id`, `instance_id`, `data_volume_id`,
`security_group_id` and `role_name`.

## Test

`make test-control-host-aws` runs plan checks with a mock provider. It needs no AWS account.

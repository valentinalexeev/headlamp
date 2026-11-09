# ADR 0001: node-control stops AWS exit nodes by scaling their Auto Scaling group

Status: accepted (decided by the agent, as a technical choice under CLAUDE.md section 1a). Refs #99.

## Context

`terraform/modules/exit-node-vm/aws` runs each exit node as one instance in an Auto Scaling group. The group has
`min_size`, `max_size` and `desired_capacity` equal to 1. A reclaimed Spot instance then comes back. #99 asks
node-control to start and stop these nodes.

`ec2:StopInstances` does not work for this layout:

- the EC2 health check of the group sees a stopped instance as unhealthy. The group replaces it with a new instance,
  so the stop undoes itself within minutes;
- a one-time Spot instance cannot be stopped at all (only Spot instances with `persistent` requests can).

An instance that is not in a group (made by hand, or by another module) can be stopped and started as usual.

## Decision

The `aws` provider handles both shapes:

1. It finds the instance by its `Name` tag (or a pinned id) as before. If the instance carries the tag
   `aws:autoscaling:groupName`, or the node block names a group (`{"asg": "<name>"}`), it controls the **group**:
   - stop sets the desired capacity and `MinSize` to 0 (one `UpdateAutoScalingGroup` call), so the group terminates
     the instance and does not replace it;
   - start sets `MinSize` and the desired capacity back to 1;
   - status is `STOPPED` when the group wants 0 and has no instance in service, `STARTING`/`STOPPING` while it moves, and
     the instance state otherwise.
2. Otherwise it calls `StartInstances`/`StopInstances` on the instance.

After a stop, the node has no instance. The provider then finds the group from the `asg` key in the block. If there is
no `asg` key, it uses the group name that the module makes, `<name>-asg`.

## Consequences

- A stopped group-managed node has **no instance**: the next start creates a new one, with a new machine key. A reusable
  key and the same hostname are **not** enough (#213). Headscale registers a new node, for example `cobalt-1`, with a
  new id and IP. The old node stays as an offline record.
- From 0.14.0, `state_parameter_name` keeps the identity (#213). tailscaled then keeps its state in SSM Parameter
  Store. The new instance loads the same machine key, so it is the same node, with the same id, name and IPs.
  Without it, delete the stale node and rename the new one after each start.
- The IAM policy needs `autoscaling:DescribeAutoScalingGroups` and `autoscaling:UpdateAutoScalingGroup`, the second
  limited by a tag on the group (documented in the node-control README).
- Stop is not instant: the group terminates the instance asynchronously. The provider reports `STOPPING` until it is gone.
- Plain instances keep the cheaper stop/start, with their disk and identity intact.

Alternatives not chosen:

- Suspend the `HealthCheck` and `ReplaceUnhealthy` processes of the group, then stop the instance. This is fragile, and
  it is not possible for one-time Spot instances.
- Standby mode (`EnterStandby`). The instance continues to run, and it is billed.

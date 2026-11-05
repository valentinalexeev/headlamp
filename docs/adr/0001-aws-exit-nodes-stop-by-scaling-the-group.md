# ADR 0001: node-control stops AWS exit nodes by scaling their Auto Scaling group

Status: accepted (decided by the agent, as a technical choice under CLAUDE.md section 1a). Refs #99.

## Context

`terraform/modules/exit-node-vm/aws` runs each exit node as one instance in an Auto Scaling group with `min_size`,
`max_size` and `desired_capacity` equal to 1, mostly so a reclaimed Spot instance comes back. #99 asks node-control to
start and stop these nodes.

`ec2:StopInstances` does not work for this layout:

- the group's EC2 health check treats a stopped instance as unhealthy and replaces it with a new, running one, so a stop
  undoes itself within minutes;
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

The group of a node with no instance (after a stop) is found from the block's `asg`, or by the group name the module
uses, `<name>-asg`.

## Consequences

- A stopped group-managed node has **no instance**: the next start creates a new one. Its tailnet identity survives only
  if the node re-joins with a reusable key (the module's default) under the same hostname. Headscale then shows the same
  node name; a stopped node stays listed while headscale keeps it.
- The IAM policy needs `autoscaling:DescribeAutoScalingGroups` and `autoscaling:UpdateAutoScalingGroup`, the second
  limited by a tag on the group (documented in the node-control README).
- Stop is not instant: the group terminates the instance asynchronously. The provider reports `STOPPING` until it is gone.
- Plain instances keep the cheaper stop/start, with their disk and identity intact.

Alternatives not chosen: suspending the group's `HealthCheck` and `ReplaceUnhealthy` processes and stopping the instance
(fragile, and impossible for one-time Spot); standby mode (`EnterStandby` keeps the instance running and billed).

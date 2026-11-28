---
issue: 431
type: feature
section: Added
status: done
breaking: false
title: A reachability diff of a draft policy in the policy console
components: [node-control]
---
## Summary

node-control: the policy console preview now shows a reachability summary under the text diff, for example
"Reachability: 3 new paths, 1 removed path", and lists the added and removed edges and group members with their
rule references. A path is an edge with each group replaced by its members, so a new group member counts too. The
new API is `POST /policy/api/map/preview` (`policy.view`, same body as the preview, limit `POLICY_MAX_BYTES`): it
returns the graph of the draft and its edge diff against the live policy. It changes nothing and never logs policy
text. See the node-control README, section "Policy map".

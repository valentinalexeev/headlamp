---
issue: 432
type: feature
section: Added
status: done
breaking: false
title: An Overview item for each policy selector that matches no node
components: [node-control]
---
## Summary

node-control: with the policy console on (`POLICY_DB`), the Overview shows one `info` attention item for each tag,
group or user of the live policy that matches no headscale node. The item goes away when a node matches, links to the
policy map with that selector selected, and can be acknowledged. A user that is only a tag owner or a group member
gets no item. See "Selectors that match no node" in the node-control README.

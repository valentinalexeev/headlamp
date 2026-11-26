---
issue: 439
type: feature
section: Changed
status: done
breaking: false
title: Devices filters for troubleshooting
components: [node-control]
pr: 477
---
## Summary

The Devices page filters are now **Needs help** (set up but no traffic yet, or no traffic for more than 7 days),
**Not set up**, **Expiring soon** and **All** (the default). The **Active** and **Revoked** filters are removed: the
old links `?filter=active` and `?filter=revoked` open **All**. The search also finds the email of a person. The row of
a person or an owner shows counts, for example "2 devices · 1 connected today · 1 not set up". See "The Devices page
tree" in the node-control README.

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

The Devices page has a new filter **Needs help** (set up but no traffic yet, or no traffic for more than 7 days), and
it is the default. The filters **Active**, **Expiring soon**, **Not set up**, **Revoked** and **All** stay. A link
without a filter now opens **Needs help**, not **Active**: use `?filter=active` for the old view. The search also finds the email of a person. The row of
a person or an owner shows counts, for example "2 devices · 1 connected today · 1 not set up". See "The Devices page
tree" in the node-control README.

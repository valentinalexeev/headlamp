---
issue: 440
type: feature
section: Changed
status: done
breaking: false
title: Less noise in the Devices table
components: [node-control]
---
## Summary

The Devices page shows one row for each person. A device that an operator enrolled for the email of a person is in the
row of that person, marked "enrolled by an admin" (Lock does not revoke it). A waiting invitation for the email of
such an owner is in the row of that owner, not under "Invited, not signed in yet". The table has no Owner column: the
row of the person or the owner gives it. The Expires column shows days ("in 84 days"), with the date on hover. Two
devices with the same name in one row get a mark. See "The Devices page tree" in the node-control README.

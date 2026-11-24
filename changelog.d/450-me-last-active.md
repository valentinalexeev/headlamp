---
issue: 450
type: feature
section: Added
status: done
breaking: false
title: The self-enrolment page shows when each own device was last active
components: [node-control]
pr: 472
---
## Summary

`/me` has a "Last active" column for each own device. It shows the later of the last traffic and the last profile download, as a relative time with the exact UTC time on hover. "Never" means neither happened. The value can be up to about 5 minutes old. See "Last active" in the node-control manual.

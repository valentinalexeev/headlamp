---
issue: 438
type: feature
section: Added
status: done
breaking: false
title: The Devices page shows a health line for each bridge
components: [node-control]
pr: 478
---
## Summary

The enrolment Devices page has a **Bridges** strip above the table, with one line for each bridge: the sync state, whether the agent answered this page load and the time of its last answer, the egress self-test and the path check class. A line is red when the sync is pending, the agent did not answer or the egress self-test fails, and amber when the path check class is not `ok`. `GET /enroll/devices` returns the same data in `bridges`. The Traffic column header now reads "Traffic (partial)" instead of "Traffic*" when a bridge did not answer. No connection log and no client addresses are shown. See "Bridges" in the node-control manual.

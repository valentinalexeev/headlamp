---
issue: 424
type: feature
section: Added
status: done
breaking: false
title: The Nodes page shows the path check of each bridge
components: [node-control]
---
## Summary

The detail of a bridge on the Nodes page has a **Path to the control host** section: the class of the last path check run with its meaning, since when and how many runs in a row, the count of each class over the 24-hour history of the agent, and the findings of the last run. `GET /enroll/ingresses` now also returns `pathcheck_text`, the text of each class. Class names are still checked, and the text comes from node-control, never from the bridge. See "Bridge path check" in the node-control manual.

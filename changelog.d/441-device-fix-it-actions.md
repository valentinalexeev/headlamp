---
issue: 441
type: feature
section: Added
status: done
breaking: false
title: Fix-it actions on a device row of the Devices page
components: [node-control]
pr: 476
---
## Summary

A device row on the Devices page has new actions. "Rotate credential" (a set-up device) gives the device a new subscription URL; the old one stops working. "Push now" (while a bridge has an update pending, also in the sync note) sends the credentials to every bridge at once through the new route `POST /enroll/sync` (operate level of enrolment, audit action `bridge.push`). "Copy support info" copies a summary of the device (state, bridges, last traffic) with no link, URL, UUID or secret. "New link" stays on a device that is not set up.

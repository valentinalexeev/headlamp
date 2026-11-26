---
issue: 437
type: feature
section: Added
status: done
breaking: false
title: A device drawer on the Devices page for diagnosis
components: [node-control]
---
## Summary

node-control: click a device name on the Devices page to open its drawer. The drawer shows the first and last
profile download and the format that the app asked for, the traffic and the credential state of each bridge
("Has the current credential", "Update pending" or "Not known yet"), the tailnet devices of a self-enrolled person
(name, online, last seen) and a timeline (created, link issued, link used, profile downloaded, last traffic,
extended, revoked). The new API is `GET /enroll/devices/{id}` (enrolment view level). node-control now stores
`profile_format`, `created_at`, `invited_at`, `extended_at` and `revoked_at` for each device (SQLite and Redis,
carried by export and import). Devices made before this version show no time for these events.

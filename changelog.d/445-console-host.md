---
issue: 445
type: fix
section: Fixed
status: done
title: The HTTPS console accepts its own name
components: [node-control]
pr: 451
---
## Summary

The admin console on HTTPS answered `400 Invalid host header` to its own name unless `ADMIN_CONSOLE_NAME` was also
in `ENROLL_ALLOWED_HOSTS`. The TLS listener now accepts only `ADMIN_CONSOLE_NAME`, with no extra setting.
`ENROLL_ALLOWED_HOSTS` guards the plain admin listener on port 80, as before. At startup, a `listener hosts` log
line shows the names that each admin listener accepts.

## Upgrade

If you added `ADMIN_CONSOLE_NAME` to `ENROLL_ALLOWED_HOSTS` as a workaround, remove it. The console on HTTPS no
longer reads that list. With the name in the list, the plain admin listener on port 80 also answers to it.

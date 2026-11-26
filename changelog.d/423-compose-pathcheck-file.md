---
issue: 423
type: feature
section: Added
status: done
title: The compose edge can serve the test file of the path check
components: [compose]
---
## Summary

The TLS edge of `examples/compose/control` can serve the test file of the bridge path check at
`https://<HEADSCALE_DOMAIN>/<PATHCHECK_BLOB_PATH>/probe.bin`, from `${DATA_DIR}/pathcheck/probe.bin`. It is off by
default: without `PATHCHECK_BLOB_PATH` in `.env` the edge has no such route. Set `ingress_pathcheck_blob_url` of the
bridges to that URL. See "Path check test file" in the compose README.

## Upgrade

Take the new `compose.yaml` and `caddy/Caddyfile` together: the Caddyfile reads `PATHCHECK`, which `compose.yaml` sets.

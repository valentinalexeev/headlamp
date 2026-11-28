---
issue: 429
type: feature
section: Added
status: done
breaking: false
title: The policy console has a Map tab that shows the policy as an interactive graph
components: [node-control]
---
## Summary

node-control: the policy console has a new tab, `/policy/map` (`policy.view`, read-only, active with `POLICY_DB`). It
draws the live policy as a graph with the vendored Cytoscape.js 3.35.0 core (MIT). You can pan, zoom and drag, switch
between the `cose` and `breadthfirst` layouts, search names, ports and protocols, click a node to highlight its
neighbours, see tooltips with the rule references and line numbers, and filter by kind. A table view shows the same
paths as text. `make check-vendored` (part of `make lint`) pins the version and SHA-256 of the vendored file.

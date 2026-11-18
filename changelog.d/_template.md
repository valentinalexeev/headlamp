---
issue: 0                  # required: the issue number; for several, use "issues: [382, 388]" instead
type: fix                 # required: feature | fix | task | security | docs | deprecation | removal
section: Fixed            # required: Added | Changed | Deprecated | Removed | Fixed | Security
status: done              # required: planned | in-progress | done | dropped
breaking: false           # optional: true needs a "## Upgrade" section
title: One line that names the change
components: [node-control]  # optional: the component names, as in the commit scope
pr: 0                     # optional: remove the line if you do not know the PR number yet
---
## Summary

The changelog text, 1 to 8 lines, for an operator who upgrades: what changed, the setting names, the manual.

## Upgrade

Optional. What a site repo must do or check for this change. It goes into the release's section of UPGRADING.md.
Required when breaking is true. Without any Upgrade part in a release, UPGRADING.md says "No action required."

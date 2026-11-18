# ADR 0005: A policy map in node-control

Status: proposed. Issue #406 tracks the work. Open points 1 and 3 wait for the owner. Point 2 is decided.

## Credit

The idea comes from [tailscale-network-topology-mapper](https://github.com/SimplyMinimal/tailscale-network-topology-mapper)
by SimplyMinimal (Apache-2.0). That tool reads a Tailscale ACL and grants policy and writes an interactive HTML map of
who can reach what. These ideas come from it:

- a graph of the policy, with one node for each group, tag, user and host, and one arrow for each "source can reach
  destination" pair;
- node colour by kind (group, tag, host) and node shape by rule type (ACL only, grant only, both);
- the ports and protocol on each arrow, and the rule and its line number in the tooltip;
- a search that matches names, ports and protocols and highlights the matches, and a click that highlights the
  neighbours of a node.

This proposal takes no code, markup or assets from that project. node-control uses Go and its own console pages.
It also has a strict Content-Security-Policy. Thus a copy would not fit. The README section of the feature and the source files
of the map credit the project with the same link. If a later change copies code from it, that change adds the
Apache-2.0 licence text and a `NOTICE` entry first.

## Context

The policy console (`internal/policy`, `/policy`) edits the headscale policy as text. Its preview shows a text diff
and a few warnings (removed rules, unknown tags, unknown users). A text diff does not show what a change does to
reachability. For example, a new group member or a changed `autogroup:internet` rule gives new paths, but the diff
shows one changed line.

The upstream tool reads only the policy file. A live view of devices is on its roadmap. node-control already has the
live data from the headscale API:

- the policy (`GetPolicy`);
- the nodes with tags, users, addresses, online state and routes (`ListNodes`);
- the users (`ListUsers`).

Thus node-control can do more than the upstream tool. It can put the real nodes on the map. It can also map a draft
policy before apply.

Limits of the admin pages that the design must keep:

- `ui.PageCSP`: scripts and styles only from the files of the page. No inline script, no style attributes, no CDN.
- The console never logs policy text.
- The headscale policy can be HuJSON (comments, trailing commas). The funnel module refuses such a policy for writes.
  A read-only map must accept it.
- New features are off by default before 1.0.0 (`VERSIONING.md`).

## Decision (proposed)

### 1. Where it lives

A "Map" tab of the policy console: `/policy/map`, in the `policy` module. It is active only when the console is
active (`POLICY_DB`). It needs `policy.view`, the same as the console. It is read-only: it opens no port, changes no
access rule and adds no protocol. Thus it needs no new variable. (Open point 1.)

### 2. The graph is built in Go, on the server

A new package `internal/policymap` turns policy text into a graph. The page only draws it.

- Parse with `github.com/tailscale/hujson` (already in `go.sum` through `tailscale.com`; it becomes a direct
  dependency). `hujson.Parse` keeps the byte offsets, so each rule gets its line number, as in the upstream tool.
  `Standardize` gives plain JSON for the rest of the work.
- Read `groups`, `tagOwners`, `hosts`, `acls`, `grants` (when present) and `autoApprovers`. Each ACL or grant rule
  gives one edge for each source and destination pair. An edge keeps the ports, the protocol, `via` and the
  posture of a grant, and a list of rule references (`acls[3]`, line 42).
- Node kinds: `group`, `tag`, `user`, `host`, `cidr`, `autogroup`. `autogroup:internet` is a special destination
  ("the internet through an exit node"), because exit nodes are the centre of this project.
- headscale validates the policy (`CheckPolicy`). The map does not try to be a second validator. Some input is
  unknown to the map, for example an unknown key or selector form. The map shows a warning for it and does not fail.
- Limits: the input size is `POLICY_MAX_BYTES`. The graph has a fixed cap on nodes and edges (for example 2000 edges).
  Above the cap the response says `truncated: true`, and the page asks the operator to filter.

### 3. Real nodes on the map (the part that the upstream tool does not have)

With "Show devices" on, the server also calls `ListNodes` and `ListUsers`. It resolves each selector to the real
headscale nodes:

- a tag to the nodes that have it (both tag models, as in the tasks module);
- a user or a group to the nodes of those users;
- `autogroup:member` and `autogroup:tagged` to their sets.

The page then shows:

- the number of nodes behind each selector, and the selectors that match no node ("dead" tags and groups);
- online or offline state, exit nodes, and approved routes;
- a question form: "What can node X reach?" and "Who can reach node X?". The answer is a list of edges with their
  rule references.

The API sends node names, not keys, and the response has `Cache-Control: no-store`.

### 4. Reachability diff in the preview

`POST /policy/api/map/preview` takes the draft text, with the same body as the preview. It returns the graph of the
draft and an edge diff against the live policy. The diff has the added, removed and changed edges, each with its rule
reference. The console preview shows a short summary under the text diff, for example "3 new paths, 1 removed path".
A link opens the map in diff mode: new edges in one colour, removed edges dashed.

This adds a check to the confirm-or-revert flow. It does not replace the headscale check.

### 5. Drawing it under the CSP

The owner wants an interactive graph. No graph library from a CDN. The ways that were compared:

- **(a) Own SVG, layered layout.** Sources in the left column, destinations in the right column. Small and with no
  dependency, but pan, zoom, drag, a force layout and grouping would all be our own code. Not chosen.
- **(b) Embed [Cytoscape.js](https://js.cytoscape.org/) (MIT) (proposed).** One vendored file, served by the page as a
  static asset under `PageCSP`.

What (b) gives:

- pan, zoom (wheel and pinch), drag of nodes, box selection and touch input;
- built-in layouts that the operator can switch at run time. `cose` (force-directed) is the default.
  `breadthfirst` is the "who → what" view. `concentric`, `circle` and `grid` are also available;
- compound nodes: a group drawn as a box around its users, a tag as a box around its devices (step 3);
- selectors and classes for highlight, dim, search and diff mode, instead of own graph code;
- canvas drawing that stays fast with some thousand elements, where SVG gets slow;
- graph functions in the browser (neighbourhood, paths) for fast highlight. The server stays the source of truth for
  the "what can X reach" answer.

Rules for the vendored file:

- Core only, no layout extensions. One pinned release in `internal/policy/static/vendor/cytoscape.min.js`, with its
  `LICENSE` next to it. A `make` check in CI compares its version and SHA-256, so a changed file fails CI.
- `make vulncheck` and Dependabot do not see it. The `upstream-versions` workflow, or a new issue each quarter,
  reminds us to update it.
- The first pull request proves in a headless browser test that the page runs under `PageCSP` with no CSP violation.
  Cytoscape.js sets styles through the CSSOM, which `style-src 'self'` allows; the test must confirm this.
- The canvas cannot read CSS variables. The page reads the colour tokens of `hl.css` with `getComputedStyle` and
  sets the graph style again when the theme changes.

The page also has a table view (the same edges as rows, sortable and searchable). A screen reader cannot read the
canvas, so the table view is required. It also works on a narrow phone screen and is the fallback when the graph is
truncated.

Interactions from the upstream tool:

- search with highlight;
- a click on a node highlights its neighbours;
- a tooltip with ports, protocol, `via`, posture and rule references with line numbers;
- a legend.

Added here: a filter by kind and "devices on/off". A click on a rule reference opens the console editor at that line.

### 6. API

| Method and path | Level | Answer |
|---|---|---|
| `GET /policy/api/map?devices=0\|1` | `policy.view` | the graph of the live policy |
| `POST /policy/api/map/preview` | `policy.view` | the graph of the draft and the edge diff |

Graph JSON: `{nodes: [{id, kind, label, rule_types, devices}], edges: [{from, to, proto, ports, via, posture,
refs: [{section, index, line}]}], warnings: [], truncated, updated_at}`. The names use snake_case, as the other
answers of the console. The node-control README, section "Policy map", has the full list of fields and limits.

### 7. Tests

- Go table tests of `internal/policymap` with policies in `testdata`, with placeholder names only. Cases: HuJSON
  comments, ACL and grants together, groups in groups, `autogroup:*`, `hosts`, CIDRs, line numbers, the cap, an edge
  diff.
- Handler tests: the `policy.view` gate, the Host guard, `POLICY_MAX_BYTES`, no policy text in the logs.
- A JS self-test of the layout and search, like `policy_selftest.js`.
- `make check-manuals` on the new README section.

## Plan

| Step | Content |
|---|---|
| 1 | `internal/policymap` (parse, graph, line numbers, edge diff) and `GET /policy/api/map`, with tests |
| 2 | The Map tab: vendored Cytoscape.js with its checksum check and CSP test, `cose` and `breadthfirst` layouts, table view, search, tooltips, legend |
| 3 | Real nodes: resolve selectors, "dead" selectors, the "what can X reach" question |
| 4 | Edge diff in the console preview, and the map in diff mode |
| 5 | Optional: an Overview attention item for selectors that match no node |

Each step is one pull request with a `CHANGELOG.md` line and the README section "Policy map".

## Consequences

- Operators see the effect of a policy change before they confirm it, not only the changed text.
- One more parser of the policy format. headscale stays the authority. The map shows a warning for input that it
  cannot read. It never blocks an apply.
- One embedded frontend library (Cytoscape.js core) to keep current by hand, with a checksum check in CI.
- The map shows the full shape of the tailnet to anyone with `policy.view`. That is the same data the console already
  shows to them as text.

## Open points for the owner

1. **Activation.** As proposed: on with the policy console, no new variable, because it is read-only. Other choice: a
   separate `POLICY_MAP=1`. Not blocking.
2. **Drawing.** The owner prefers an interactive graph (chat, 2026-11-15). As proposed: (b) embed Cytoscape.js core.
   Recorded on #406.
3. **Grants.** As proposed: parse `grants` when present. Show a warning for each grant field that is not
   supported by the headscale version in `HEADSCALE_VERSION`. Not blocking.

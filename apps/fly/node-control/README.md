# node-control (Go)

A small web UI and JSON API to start, stop and inspect your exit nodes, plus optional user enrolment for a
REALITY ingress proxy. It is a single static Go binary that embeds a tailscale node (`tsnet`), joins your
headscale tailnet and listens **only on its tailnet address**: `:80` admin, and `:8081` public enrolment when
`ENROLL_DB` is set. Nothing is exposed through Fly's proxy (`fly.toml` has no `[[services]]`).

It replaces the earlier Python implementation with the same HTTP API and the same enrolment database format.
Providers: Yandex Compute, OCI Compute, Selectel Cloud, Fly Machines.

## Environment

Secrets go in Fly secrets; plain settings in `fly.toml`.

| variable | purpose |
|---|---|
| `HEADSCALE_LOGIN_SERVER` | headscale URL: control server for the embedded node and base of the node API |
| `HEADSCALE_API_KEY` | secret; lists nodes for discovery and the tailnet status. Without it `/tailnet/status` answers `not_configured` and only explicitly configured nodes exist |
| `TAILSCALE_AUTH_KEY` | secret; pre-auth key for this app's own tailnet identity (tags come from the key, not from the client) |
| `TAILNET_HOSTNAME` | name on the tailnet, default `node-control` |
| `TSNET_VERBOSE` | `1` forwards the embedded tailscale node's own logs (chatty); by default only user-facing messages such as the login URL are logged. At start the app waits up to 60 s for the node to join the tailnet and exits non-zero if it cannot (bad auth key, unreachable control server) |
| `REDIS_URL` | secret (Fly secret only, never in `fly.toml`); plain `redis://` URL of the shared Redis on Fly's private network. When set, the tailscale state lives in Redis instead of a file (see "Tailscale state"). Unset: file state under `/var/lib/tailscale`, as before |
| `REDIS_KEY` | optional hash key for the state, default `<FLY_APP_NAME or TAILNET_HOSTNAME>:tsnet` (for example `node-control:tsnet`). Must look like `<app>:<name>` and contain no `* ? [` |
| `NODE_CONTROL_STATE_KEY` | secret; 32 random bytes, base64 (`openssl rand -base64 32`). Encrypts the Redis state (AES-256-GCM). Required with `REDIS_URL` unless `STATE_ALLOW_PLAINTEXT=1`; ignored (with a warning) without `REDIS_URL` |
| `STATE_ALLOW_PLAINTEXT` | `1` stores the Redis state unencrypted. Explicit opt-out; not recommended, the state holds node private keys |
| `STATE_IMPORT_FROM` | optional path of an existing tailscaled/tsnet state file (for example `/var/lib/tailscale/tailscaled.state`); imported once on start when the Redis hash is empty, otherwise ignored |
| `NODES_CONFIG` | optional overrides file, default `/srv/app/nodes.json`; a missing file is fine, a malformed one stops startup |
| `ENROLL_ALLOWED_HOSTS` | optional, comma-separated Host values the admin listener accepts (anything else gets 400); set it to block DNS rebinding. `*` allows any host and `*.example.com` any host ending in `.example.com` (list `example.com` too for the bare name), as in the Python app |
| `ENROLL_DB` | path of the SQLite database; enables enrolment. Put it on the state volume: `/var/lib/tailscale/enroll.db` |
| `ENROLL_CONFIG` | enrolment ingress description, for example `/srv/app/enroll.json` (required with `ENROLL_DB`) |
| `ENROLL_SECRET_KEY` | secret, at least 16 bytes; derives subscription secrets (required with `ENROLL_DB`) |
| `AGENT_URL`, `AGENT_TOKEN` | address (dialled through the tailnet) and bearer token of the ingress agent (required with `ENROLL_DB`) |
| `POLICY_DB` | path of the SQLite change history; enables the policy console (`/policy`). On the state volume: `/var/lib/tailscale/policy.db`. Needs `HEADSCALE_LOGIN_SERVER` and `HEADSCALE_API_KEY` |
| `POLICY_ADMIN_USERS` | comma-separated tailnet logins allowed to use the console (required with `POLICY_DB`); tagged nodes are never allowed |
| `POLICY_REVERT_SECONDS` | seconds to confirm a change before it is reverted automatically, 30 to 900, default 120 |
| `POLICY_MAX_BYTES` | largest policy accepted, 1024 to 4194304, default 262144 |
| `YANDEX_SA_KEY_JSON`, `YANDEX_FOLDER_ID` | Yandex service account key; folder for the lookup by name (the account needs `compute.operator` and `compute.viewer`) |
| `OCI_USER_OCID`, `OCI_TENANCY_OCID`, `OCI_FINGERPRINT`, `OCI_PRIVATE_KEY`, `OCI_REGION`, `OCI_COMPARTMENT_ID` | OCI credentials and the default region and compartment |
| `SELECTEL_ACCOUNT_ID`, `SELECTEL_PROJECT_ID`, `SELECTEL_USER`, `SELECTEL_PASSWORD`, `SELECTEL_REGION`, `SELECTEL_AUTH_URL` | Selectel (OpenStack) service user, default region, optional auth URL |
| `FLY_API_TOKEN` | app-scoped deploy token for the controlled Fly app(s) |

## Tailscale state

With `REDIS_URL` set, the embedded node keeps its identity in one Redis hash (field = tailscale state key,
default hash `node-control:tsnet`) instead of a file, and the values are encrypted at rest with
`NODE_CONTROL_STATE_KEY` (AES-256-GCM; the ciphertext is bound to the hash key and field, so a copy elsewhere does
not decrypt). Starting with `REDIS_URL` and no key is an error unless you set `STATE_ALLOW_PLAINTEXT=1`.

Redis rules (shared instance): plain `redis://` on the private network, database 0 only, the code sends only
`HELLO 2` (go-redis always opens with it; `HELLO 2 AUTH <user> <pass>` authenticates and keeps the connection on RESP2),
`PING`, `HGET`, `HSET`, `HDEL`, `HLEN`, `HEXISTS` and, for `-force` only, `HKEYS` on the small state hash (never `CLIENT`, `SELECT`, `FLUSH*`, `KEYS` or `SCAN`; a test records
every command), the key is prefixed with the app
name (`<app>:`) and has no TTL, and nothing polls Redis (the node writes state rarely). The connection uses the normal
OS dialer, not the tailnet. A Redis that is unreachable at start stops the app with a non-zero exit; error text never
contains the URL, the password or the key.

Carrying over an existing identity (so the node keeps its tailnet address): either set `STATE_IMPORT_FROM` to the
current state file for the first start (it is imported only while the Redis hash is empty, then ignored), or run it by
hand from `fly ssh console`:

```
node-control state-import -from /var/lib/tailscale/tailscaled.state [-force]
```

The importer reads the file's JSON (state key -> base64), writes every entry through the encryption layer, reads each
back to verify, and prints only a count. It refuses when Redis already holds state unless `-force` is given, and never
modifies the source file. After a successful import you can delete the old state file.

Interrupted import: before writing, the importer stores a marker field (`_node-control-import`, unencrypted, only the
key names, never values) in the hash and removes it last. If an import fails it rolls back what it wrote. If the process
dies midway (restart, kill), the marker stays; the next start (or `state-import`) sees it, deletes the listed keys and the
marker, and then either redoes the import (when `STATE_IMPORT_FROM` names an existing file) or exits non-zero with a message
saying the interrupted import was cleaned up. It never starts the node on partial state. To recover, set `STATE_IMPORT_FROM`
to the old state file and restart, or run `node-control state-import -from <file>`. State written by the node itself has no
marker and is always treated as complete.

Moving from plaintext to encrypted Redis state: the encrypted store refuses plaintext values. Either export the state to a
file in the JSON format above (field name -> base64 value) and run `state-import -from <file> -force` with the key set
(`-force` replaces the whole identity: the marker lists the file's keys plus every key already in the hash, the old extra keys are deleted before the new ones are written, and a failed or interrupted forced import leaves no real keys at all; the previous identity is gone and the error says so), or delete the hash and let the node register
again with `TAILSCALE_AUTH_KEY` (it then gets a new identity).

What is **not** in Redis: the SQLite enrolment database (`ENROLL_DB`) stays on the volume, because it is a database
file and a Machine mounts one volume.

## Node discovery

There is no per-node list to maintain. Each headscale node is looked at on every refresh (cached for 15 seconds, one
headscale fetch shared by the node list and the tailnet status):

1. A node is listed if it has the tag `tag:exit`. Its UI and API id is its reported `name`.
2. If it also has `tag:cloud-<provider>` (`yandex`, `oci`, `selectel`, `fly`), that provider locates the VM whose name
   equals the node name: OCI by name in the compartment, Selectel by name in the project and region, Yandex by name in
   the folder, Fly by app name. Found means controllable; not found shows as `error` on that node only. Without a cloud
   tag the node is status-only.
3. Provider settings are per provider and come from the environment (table above), not from the node.
4. Headscale keeps offline nodes, so a stopped VM stays listed and can be started. A node deleted from headscale
   disappears.
5. Duplicate registrations (`name-1` next to `name`) fold into one entry; the online one wins.

### Optional overrides: `nodes.json`

The file is optional. Existing Python-era files keep working. Start from
[`testdata/nodes.example.json`](testdata/nodes.example.json); the image ships no `nodes.json` (the page then uses the default title); lay yours over `app/` at deploy time.

- `title`, `subtitle`: page heading.
- `nodes.<id>`: an override for a discovered node, or an explicit node. Keys: `label` (UI text), `hidden` (`true`
  removes it from the page and API), `tailnetName` (its headscale name, default the id), `provider` plus a block named
  after the provider with connection details (explicit blocks win over discovery). Blocks:

  | provider | block |
  |---|---|
  | `yandex` | `{"instance_id"}` |
  | `oci` | `{"region", "compartment_id", "instance_name"}` or `{"instance_id"}` |
  | `selectel` | `{"region", "instance_name"}` or `{"instance_id"}` |
  | `fly` | `{"app"}` |
- `aliases`: `{"old-id": "node-name"}` so bookmarks and scripts using an old id keep working.
- `tailnetTrack`: extra headscale names to include in `/tailnet/status`.

## API

`GET /` (page) · `GET /nodes.json` · `GET /status` · `GET /nodes/{id}/status` · `POST /nodes/{id}/start` ·
`POST /nodes/{id}/stop` · `GET /tailnet/status` · `GET /healthz`. With `ENROLL_DB` also the `/enroll` admin pages and
API on `:80` and the public `/e/<token>` and `/sub/<secret>` on `:8081`. Start and stop are idempotent; a provider failure
shows up as `error` on that node only.

## Enrolment (optional)

Setting `ENROLL_DB` turns on user enrolment for a REALITY ingress proxy. The binary then serves a second, public
enrolment listener next to the admin app. Both bind to the tailnet address only:

| port | serves | reachable by |
|---|---|---|
| 80 | admin app, including the `/enroll` admin pages | operators on the tailnet |
| 8081 | public enrolment | the ingress proxy only (restrict it in the tailnet ACL) |

Environment (secrets via Fly secrets, the rest in `fly.toml`):

- `ENROLL_DB`: path of the SQLite database: `/var/lib/tailscale/enroll.db`. It must live on a Fly volume or it is
  lost on deploy, and a Fly Machine mounts only one volume, so use the existing `tailscale_state` volume (mounted
  at `/var/lib/tailscale`, see above); do not create a second one. That volume's snapshots then also hold the
  enrolment database (device names and owners, not the subscription secrets, which are derived from
  `ENROLL_SECRET_KEY`); treat them accordingly. Back it up with `sqlite3 ... .backup` before risky changes.
- `ENROLL_CONFIG`: path of a JSON file describing the ingress, for example
  `{"host": "ingress.example.com", "public_key": "<reality-public-key>", "short_id": "<short-id>", "sni": "ingress.example.com"}`,
  optionally with `"port": 443` (the client's `server_port`, default 443) and `"profile_name": "tunnel"` (the label
  of the imported profile, default `tunnel`). `sni`, `short_id` and `public_key` must equal the `reality-ingress`
  role's `reality_sni`, `reality_short_id` and the public half of `reality_private_key` (`xray x25519 -i <private>`);
  `port` must equal its `ingress_listen_port`. Keep the real file in your private repo and overlay it.
- `ENROLL_SECRET_KEY`: at least 16 bytes; derives the subscription secrets. Changing it invalidates existing subscriptions.
- `AGENT_URL`, `AGENT_TOKEN`: address and bearer token of the ingress agent that applies the user changes.

- `ENROLL_ALLOWED_HOSTS` (optional): comma-separated Host header values the admin app accepts, for example
  `node-control.<tailnet-domain>,<tailnet-ip>`. Any other Host gets `400` (the same rules as Starlette's `TrustedHostMiddleware`, applied
  to the whole admin app on port 80). Set it: it stops a malicious web page from reaching the admin API through DNS
  rebinding in an operator's browser. List every name/IP you open the page with.

### Sync with the ingress host

Every create/revoke/extend pushes the full credential set to the agent; a reconcile loop retries every 5 minutes.
A failed push is logged (exception class, and the HTTP status for HTTP failures; never payloads or credentials) and
classified: `unreachable` (no connection), `unauthorized` (401/403, `AGENT_TOKEN` mismatch), `rejected` (other
non-2xx, typically 503: xray refused the generated config), `conflict` (409), `error` (unexpected exception in the
loop). `GET /enroll/devices` returns `{"devices": [...], "sync": {"pending": bool, "state": "...", "since": <unix>|null}}`;
create/revoke/extend responses carry `sync_pending` and `sync`. While `pending`, the `/enroll` page shows a banner
"ingress host out of sync since <time>: <state>": until it clears, revocations and extensions are not in effect on
the ingress host. A store with no devices at all is never pushed (state `empty`): the agent would refuse an
unconfirmed empty set, and an empty store must never wipe the agent.

### What the ingress host sees

The one-time link (`/e/<token>`), its redirect and the subscription (`/sub/<secret>`) all pass through the ingress
host's Caddy front. None of them carries the device name or owner: the redirect's deep link uses the neutral
`profile_name` as the profile label.

### Link previews

Messengers fetch a pasted link to build a preview, which would use up a single-use link before the person taps it.
`/e/<token>` and `/sub/<secret>` answer a plain empty `404` (and do not consume the token) when the User-Agent
contains, case-insensitively, one of: `facebookexternalhit`, `twitterbot`, `telegrambot`, `whatsapp`, `slackbot`,
`discordbot`, `skypeuripreview`, `linkedinbot`, `embedly`, `bot`, `crawler`, `spider`, `preview`, `linkexpanding`,
`google-` (iMessage previews identify as `facebookexternalhit`/`Twitterbot`). Browsers (Safari, Chrome) and the
client apps pass; an empty or missing User-Agent counts as a normal client. A preview bot with an unlisted
User-Agent can still burn a link: create another one.

### Phone profile

The profile is a sing-box config: tun (`172.19.0.1/30`, `fdfe:dcba:9876::1/126`, auto/strict route), a selector
`select` over the `auto`/`direct`/`exit` VLESS+Reality outbounds, DNS hijacked on the tun and resolved by
`tls://1.1.1.1` through the selected outbound (`detour: select`, sing-box legacy DNS format, valid through 1.13).
The ingress hostname (the VLESS outbounds' `server`) is the exception: a DNS rule `outbound: any` resolves it with the
phone's local resolver (`local` server, no detour), since resolving it through `select` would need the outbound that
needs the lookup. All other lookups go through the selected outbound. That this works, and that only the ingress
hostname reaches the local resolver, is verified only in the phone test:
in `exit` mode a site that is unreachable from the client's own network must open, and a DNS leak test must show only the exit's resolver.

Differences from the Python app this replaced: the agent is dialled through the embedded tailnet node, and the
database is closed only after both listeners have drained on shutdown. A misconfigured enrolment (a missing variable,
an unreadable or invalid `ENROLL_CONFIG`) stops startup with a message naming the problem, never a secret.

## Navigation bar

Every admin page shows a small top bar that lists the modules available in this deployment, with the current one
highlighted. Today that is Nodes (`/`) and, when `ENROLL_DB` is set, Enrolment (`/enroll`). The bar appears only
when at least two entries exist, so a deployment without enrolment looks exactly as before. It is never added to
the public listener, to JSON, to non-200 responses or to any request other than GET and HEAD.

The core inserts the bar into HTML responses of the admin listener, inside the Host guard. `GET /_nav.json` returns
the same list as `[{"label":"Nodes","path":"/","order":10},...]` with `Cache-Control: no-store`.

A module with an admin page adds an entry by implementing the optional `core.Navigator` interface. The core
collects entries after every module registered, so a module that is switched off (return `nil`) or failed to
register never appears:

```go
func (m *Module) Nav() []core.NavItem {
	return []core.NavItem{{Label: "Reports", Path: "/reports", Order: 30}}
}
```

A label is 1 to 24 printable characters (no control, bidi or zero-width characters); a path is a local absolute
path (`/x`, not `//x`, `http://x`, `javascript:x`, and without `?`, `#`, quotes or angle brackets). The bar is fixed
at the top and uses the page's own colour variables; the longest matching path is the current one. Invalid entries and duplicate paths stop the start-up with an error naming the module.

## Policy console (optional)

Setting `POLICY_DB` serves `/policy` on the admin listener for the logins in `POLICY_ADMIN_USERS`: edit the headscale
policy, preview the diff, apply, and confirm. An applied change reverts by itself after `POLICY_REVERT_SECONDS` unless
it is confirmed, which also covers a restart of this app mid-window. Every route is refused (403) for other users and
for tagged nodes; policy text is never logged. Headscale must run its policy in database mode.
`ENROLL_ALLOWED_HOSTS` is required with `POLICY_DB` and must not contain a bare `*` (a `*.example.com` suffix is fine):
the core's Host guard is what protects the console from DNS rebinding. Confirm names the pending change and succeeds
only while headscale still serves that change's policy. If the automatic revert keeps failing, the page shows a warning;
Confirm is then the way out, keeping the change.
The console adds a "Policy" entry to the top bar. Its Content-Security-Policy allows inline styles (the bar is one) but scripts only from the
page's own files.

## Build and run

```
make build                 # bin/node-control, CGO off
docker build -t node-control .
```

The image is `alpine` (a shell stays available for `fly ssh console`) with the binary at
`/usr/local/bin/node-control` and `app/` at `/srv/app`. `ARG TSVERSION` only documents the tailscale.com release and
fails the build if `go.mod` pins another one. State lives on the volume mounted at `/var/lib/tailscale` (see `fly.toml`).
Deploy with the overlay procedure from the repository `CLAUDE.md` (section 2.5).

## Tests

```
go vet ./... && go test ./...
go test -race ./...
```

No cloud access is needed: providers and headscale are fake HTTP servers, and the core runs on plain TCP instead of
tsnet.

## Trial and cutover helpers

Both scripts only read.

- `scripts/parity.sh <python-url> <go-url>` fetches `/nodes.json`, `/status`, `/tailnet/status` and `/enroll/devices` from
  both apps, normalises JSON with `jq -S` and prints a unified diff; exit 1 on any difference. Needs `curl` and `jq`.
- `scripts/measure-memory.sh <fly-app> [seconds=300] [status-url]` samples the process RSS and `MemAvailable` every 10 s
  over `fly ssh console` while requesting `status-url`, then prints min `MemAvailable`, max RSS and PASS/FAIL against:
  `MemAvailable` above 20% of `MemTotal` and RSS below 205 MB. Run it on the trial app before cutover.

`measure-memory.sh` finds the process by its executable (`/usr/local/bin/node-control`, via `scripts/probe.sh`), requires
exactly one match and prints the pid and `comm` in every sample. `sh scripts/test_measure_memory.sh` tests that logic
and the argument checks without `fly`.

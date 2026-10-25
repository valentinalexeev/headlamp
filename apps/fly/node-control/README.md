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
| `ENROLL_CONFIG` | enrolment ingress description, for example `/srv/app/enroll.json` (required with `ENROLL_DB`; shape under "Enrolment") |
| `ENROLL_SECRET_KEY` | secret, at least 16 bytes; derives subscription secrets (required with `ENROLL_DB`) |
| `AGENT_URL`, `AGENT_TOKEN` | address (dialled through the tailnet) and bearer token of the first ingress's agent. Required with `ENROLL_DB` only for the old single-host `enroll.json`; in the list shape they are the optional fallback of the FIRST entry (no `agent_url` / no `AGENT_TOKEN_<NAME>`) |
| `AGENT_TOKEN_<NAME>` | one bearer token per ingress: `<NAME>` is the ingress name upper-cased with `-` as `_` (`ingress-2` reads `AGENT_TOKEN_INGRESS_2`). An entry's `token_env` names another variable instead |
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
- `ENROLL_CONFIG`: path of a JSON file describing the ingress nodes (shape below). Keep the real file in your
  private repo and overlay it.
- `ENROLL_SECRET_KEY`: at least 16 bytes; derives the subscription secrets and the per-ingress credentials. Changing
  it invalidates both (see "Credentials per ingress").
- `AGENT_URL`, `AGENT_TOKEN`: address and bearer token of the first ingress's agent (the old single-host setup).
  Further ingresses use `agent_url` in their entry (or are found through headscale) and `AGENT_TOKEN_<NAME>`.

- `ENROLL_ALLOWED_HOSTS` (optional): comma-separated Host header values the admin app accepts, for example
  `node-control.<tailnet-domain>,<tailnet-ip>`. Any other Host gets `400` (the same rules as Starlette's `TrustedHostMiddleware`, applied
  to the whole admin app on port 80). Set it: it stops a malicious web page from reaching the admin API through DNS
  rebinding in an operator's browser. List every name/IP you open the page with.

### `enroll.json`

One or more ingress nodes. Each entry is an ingress host with its own agent; a device gets a credential on every
active ingress and the phone profile lists them all.

```json
{
  "profile_name": "tunnel",
  "link_host": "link.example.invalid",
  "ingresses": [
    {"name": "default", "host": "ingress-1.example.invalid", "public_key": "<reality-public-key>",
     "short_id": "<short-id>", "uuids": "base"},
    {"name": "ingress-2", "host": "ingress-2.example.invalid", "public_key": "<reality-public-key-2>",
     "short_id": "<short-id-2>", "uuids": "derived", "agent_url": "http://192.0.2.10:9000",
     "token_env": "AGENT_TOKEN_SECOND"}
  ]
}
```

- Top level: `ingresses` (1 to 8 entries, required), `link_host` (required with more than one ingress; with one it
  defaults to that ingress's `host`), `profile_name` (label of the imported profile, default `tunnel`). Unknown keys
  are an error.
- Per entry: `name` (`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, unique), `host` (unique DNS name), `public_key`,
  `short_id`, `uuids` (required, see below). Optional: `sni` (default `host`), `port` (default 443),
  `agent_url` (`http(s)://host:port`; without it the first entry uses `AGENT_URL` and the others are found through
  headscale as the `tag:ingress` node carrying the entry's name, agent port 9000), `token_env` (name of the token
  variable, `^[A-Z][A-Z0-9_]{0,63}$`; default `AGENT_TOKEN_<NAME>`, and for the first entry also `AGENT_TOKEN`).
  `sni`, `short_id` and `public_key` must equal the `reality-ingress` role's `reality_sni`, `reality_short_id` and
  the public half of `reality_private_key` (`xray x25519 -i <private>`); `port` must equal its `ingress_listen_port`.
- `link_host` is the host of subscription URLs and one-time links. Phones fetch `https://<link_host>/sub/<secret>`
  and `/e/<token>`, so the name must resolve to an ingress whose front serves it and proxies to node-control's
  `:8081`, with a certificate for it. Prerequisite: DNS for `link_host` and a working certificate on whichever
  ingress it points at, before the first device is enrolled. Every ingress's front must be able to serve it, so
  moving the name to another ingress is enough to fail over.
- The old single-host file (`host`, `public_key`, `short_id`, `sni`, `port` at the top level, no `ingresses`) still
  loads as one ingress named `default` with `"uuids": "base"`, and uses `AGENT_TOKEN`. Mixing the two shapes is an
  error.

### Credentials per ingress (`uuids`)

`uuids` is required on every ingress in the list shape; there is no default, because a value that depended on
the other entries would change meaning when one is added or removed.

- `"base"`: the device's stored UUIDs are the credential on this ingress. At most one ingress may use it, the one that
  exists today.
- `"derived"`: the credential is computed per ingress and device mode with HMAC-SHA256 keyed by
  `ENROLL_SECRET_KEY`, so a credential taken from one ingress is useless on the others, and a compromised ingress
  cannot compute any other ingress's values.

Migration from the single-host file: write the list shape with the existing ingress as an entry with
`"uuids": "base"` (its name does not matter for `base`, `default` or any other) and every new ingress as `"derived"`.
Existing phones keep working untouched, because their credential on the old ingress did not change.

Consequences to plan for:

- The ingress NAME is part of a derived credential. Renaming a `derived` ingress rotates its credentials: phones need a
  new profile. Reordering entries does not change any credential, but the FIRST entry owns the `AGENT_URL` /
  `AGENT_TOKEN` fallback and the link view (key, short id and sni used for import links), so moving another entry to
  the top changes which agent those variables reach.
- Rotating `ENROLL_SECRET_KEY` invalidates the subscription secrets AND every derived credential. Re-publish
  and re-enrol the profiles. `base` credentials survive.
- Removing an entry from the file stops syncing to it: that ingress keeps its last credential set, and a later
  revocation does not reach it. Take the host down or wipe its agent first.

### Sync with the ingress host

Every create/revoke/extend pushes the full credential set to the agent; a reconcile loop retries every 5 minutes.
A failed push is logged (exception class, and the HTTP status for HTTP failures; never payloads or credentials) and
classified, per ingress: `unreachable` (no connection), `unauthorized` (401/403, `AGENT_TOKEN` mismatch), `rejected` (other
non-2xx, typically 503: xray refused the generated config), `conflict` (409), `error` (unexpected exception in the
loop). `GET /enroll/devices` returns `{"devices": [...], "sync": {"pending": bool, "state": "...", "since": <unix>|null}}`;
create/revoke/extend responses carry `sync_pending` and `sync`. While `pending`, the `/enroll` page shows a banner
"ingress host out of sync since <time>: <state>": until it clears, revocations and extensions are not in effect on
the ingress host. A store with no devices at all is never pushed (state `empty`): the agent would refuse an
unconfirmed empty set, and an empty store must never wipe the agent.

Every ingress has its own sync state and gets its own credential set (the derived ones for a `derived` ingress). A
failure on one ingress never fails the operation: a create, revoke or extend is recorded first and answers success,
the response and the page show the ingress that is behind (`sync` aggregates, `GET /enroll/ingresses` has one row per
ingress), and the reconcile loop retries it. Revoking therefore never blocks on an unreachable ingress; until that
ingress is back, the revoked credential still works there. The banner and the per-ingress rows tell which one.
Traffic figures are merged per device across the ingresses; when an ingress with a pending sync answered nothing,
`stats_partial` is `true` and the page marks the column `Traffic*`: the numbers are a lower bound.

### Discovered ingresses, approval and slow agents

Nodes tagged `tag:ingress` in headscale that are not listed in `enroll.json` are discovered (needs `link_host` in the
file and `HEADSCALE_API_KEY`; the discovery line on the page says why it is off) and shown on the `/enroll` page. Each
one is asked for its public parameters (`GET /v1/info` on its agent, bearer token `AGENT_TOKEN_<NAME>`, `<NAME>` the
node name). A discovered ingress is used only after the operator approves it; the approval pins the fingerprint of its
published parameters AND the headscale node id that published them. Discovered ingresses always use derived
credentials. Approvals live in the enrolment database. States of a row: `configured` (listed in `enroll.json`,
approved by definition), `pending` (never approved), `approved`, `changed`, `unreachable`, `no token`, `invalid`.

Adding an ingress, step by step:

1. Deploy the host with the `reality-ingress` and `ingress-agent` roles; give its agent its own token and set the
   `INGRESS_*` values so `/v1/info` answers. Its front must serve `link_host` (see "Compromised ingress").
2. Register the node in headscale with `tag:ingress` and a name that fits `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`
   (this becomes the ingress name). Then pin the name with `headscale nodes rename <id> <name>`, using a name that the
   machine's hostname does not produce (otherwise headscale follows a hostname change). `headscale nodes list` shows
   the id, addresses and names.
3. Set the token as a secret named `AGENT_TOKEN_<NAME>` and redeploy node-control (or use an `enroll.json` entry
   instead, see above, which needs no approval).
4. Open `/enroll`. The ingress appears as `pending` with its host, the node id, tailnet address and names, a short
   prefix of its public key and short id, a short prefix of its machine key and a fingerprint. Compare the node id,
   address, name and machine key with `headscale nodes list`: an impostor holding the name can publish the real ingress's public values, so the node
   itself must be the one you registered. The agent does not report a fingerprint: it is node-control's SHA-256 over the canonical JSON of
   `name`, `host`, `port`, `sni`, `public_key` and `short_id`. Compare the host, public key and short id that
   `GET /v1/info` on the agent reports (over a channel you trust) with what the page shows, then press Approve. The
   API is `POST /enroll/ingresses/<name>/approve` with `{"fingerprint": "<shown fingerprint>", "node_id": <shown node id>}`;
   a fingerprint that no longer matches the parameters the ingress publishes at that moment, or a node id other than
   the node that publishes them, is refused (409). `POST
   /enroll/ingresses/<name>/unapprove` takes it out again. Both POSTs need `Content-Type: application/json`.
5. Approval pushes the credential set at once; the row shows its sync state. Phones pick the new ingress up when they
   refresh their subscription; a profile imported by a one-time link needs the person to refresh it.

- A `changed` ingress (its parameters differ, or another node id publishes them) is frozen: it is out of profiles
  and no longer synced, so it keeps its old credential set until it is approved again or un-approved. A revocation
  does not reach it meanwhile.
- Names in headscale are NOT proof of who owns a node. The given name starts as the hostname the machine reports
  and headscale re-derives it whenever that hostname changes, as long as the given name still follows it (equals the
  hostname, or the hostname plus `-<digits>`). Only a name set with `headscale nodes rename <id> <name>` to something
  the hostname does not produce stays put. So:
  - **Set an explicit `agent_url` (the node's tailnet IP, for example `http://100.64.0.12:9000`) on every configured
    ingress.** It removes the headscale lookup altogether: the token goes only to that address, which no machine
    can influence. (`AGENT_URL` does the same for the first entry.) The name lookup is only the fallback, and it
    is the weaker setting.
  - For a CONFIGURED ingress without `agent_url`, the lookup accepts a tagged node only when its given name does not
    follow its hostname, by headscale's own rule (the given name equals what headscale derives from the hostname,
    optionally followed by `-<number>`; hostnames that sanitise to nothing are refused too). Run
    `headscale nodes rename <id> <name>` on the ingress node first (the log says so when a lookup is refused). When
    several nodes share the name the lookup fails and nothing is sent to any of them.
  - A DISCOVERED ingress is identified by the node id AND the machine key it was approved on, not by its name: the
    agent is found by that pair, and no other node is ever sent its token or credentials. The id alone is not enough
    (ids can repeat after a headscale database is rebuilt or restored); the machine key is what a node cannot
    choose. If the node list does not report machine keys, an ingress cannot be approved. Approvals made before
    machine keys were pinned show `changed` ("approve again") until you approve them once more. If that node disappears, the row turns `invalid`
    ("the approved node is not present") and nothing is contacted until you remove the approval; a different node that
    now carries the name can then be reviewed and approved.
- Before approval, the token variable `AGENT_TOKEN_<NAME>` is already sent (as the bearer of `GET /v1/info`) to a
  tagged node whose given name matches it. Pinning the node id after approval reduces this exposure but cannot remove
  it: anyone who can register a tagged node with that name (and, for a host with no rename, a matching hostname)
  receives that one token. Do not set the variable before you expect the node.
- A create, revoke, extend or approval answers after at most 3 seconds. If an agent has not answered by then, its row
  reads `pending` with state `unknown` until the push ends; the change itself is already recorded and never fails.

### Compromised ingress

Threat model, in short: a root attacker on one ingress host sees client source addresses and traffic per credential,
its own Reality private key, and the credentials pushed to it, and controls its own xray and agent (a revocation
does not bind it). Because TLS ends on the ingress's front, then plain HTTP goes over the tailnet to node-control, it
also sees every one-time link token and subscription secret that passes through it. It cannot reach node-control's
admin port or the other ingresses' agents (restrict both in the tailnet ACL: the agent port only from node-control,
`:8081` only from the ingress hosts), nor the tailnet through xray.

What the design limits:

- Credentials are per ingress (`derived`), so what is stolen from an ingress's agent or xray is valid on that
  ingress only. This holds for an ingress that never served `link_host`. An ingress whose front serves `link_host`
  terminates TLS for profiles and subscription secrets: it sees complete profiles, with every ingress's credentials
  (the `base` ones included), and any host tagged `tag:ingress` may fetch `:8081/sub/` with a secret it harvested. If
  such an ingress is compromised, rotate the subscription secrets and `ENROLL_SECRET_KEY` (this also changes every
  derived credential) and re-enrol the devices that held `base` credentials. The `base` ingress holds the stored
  credentials; keep it the one you trust most or migrate it to a derived entry in time.
- A discovered ingress is never used before approval, and the approval pins the fingerprint and the node id. A
  changed host is frozen out of profiles and sync until approved again.
- Each ingress is asked with its own token and is pushed only its own credential set (see the condition above for an ingress that serves `link_host`).

What it does not limit: a compromised ingress that serves `link_host` can hand phones a modified profile (clients do
not verify a signature) and read the tokens and secrets that pass through. The canary only detects this.

Detection: `scripts/profile-canary.sh <tailnet-base-url> <link-host> [<canary-sub-secret> | -]` fetches a dedicated canary
device's profile directly (`<tailnet-base-url>:8081/sub/<secret>`) and through `https://<link-host>/sub/<secret>`,
compares SHA-256 of the two bodies and prints the two short hashes and `OK` or `MISMATCH`, never the body or the
secret (exit 0 identical, 1 different, 2 usage or fetch error; each side must answer exactly 200 with a non-empty
body, a redirect or an empty answer is an error). Create a device just for it and run the script from cron or CI on a
machine on the tailnet; the secret is the canary device's `<digits>.<token>`. Give it as the environment variable
`CANARY_SECRET` or as `-` (one line on stdin): an argument shows in `ps`, and `sh -x` would print it. A false
`MISMATCH` is possible if the approved set of ingresses changes between the two fetches; run it once more before
acting. It needs `curl`
and `sha256sum` or `shasum`; `sh scripts/test_profile_canary.sh` tests it against local fakes. It only catches an
ingress that serves the canary a different profile than everybody else; one that targets other phones, that only
watches, or that recognises the canary by its fixed `User-Agent` (`profile-canary`) or its source address and serves it
the honest profile, passes.

Structural fixes (not built): serve `link_host` from a host that is not on the data path of any ingress, or pass TLS
through the front to a certificate held by node-control's side, so no ingress terminates it.

If an ingress is compromised, follow the site's incident runbook (`ingress-compromise`, kept in the private site
repository that runs this deployment): cut the host off, rebuild it, rotate what it held and re-enrol affected devices.

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

## Trial, cutover and canary helpers

These scripts only read (`profile-canary.sh` is described under "Compromised ingress").

- `scripts/parity.sh <python-url> <go-url>` fetches `/nodes.json`, `/status`, `/tailnet/status` and `/enroll/devices` from
  both apps, normalises JSON with `jq -S` and prints a unified diff; exit 1 on any difference. Needs `curl` and `jq`.
- `scripts/measure-memory.sh <fly-app> [seconds=300] [status-url]` samples the process RSS and `MemAvailable` every 10 s
  over `fly ssh console` while requesting `status-url`, then prints min `MemAvailable`, max RSS and PASS/FAIL against:
  `MemAvailable` above 20% of `MemTotal` and RSS below 205 MB. Run it on the trial app before cutover.

`measure-memory.sh` finds the process by its executable (`/usr/local/bin/node-control`, via `scripts/probe.sh`), requires
exactly one match and prints the pid and `comm` in every sample. `sh scripts/test_measure_memory.sh` tests that logic
and the argument checks without `fly`.

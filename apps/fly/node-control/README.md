# node-control (Go)

A small web UI and JSON API to start, stop and inspect your exit nodes, plus optional user enrolment for a
REALITY bridge proxy. It is a single static Go binary that embeds a tailscale node (`tsnet`), joins your
headscale tailnet and listens **only on its tailnet address**: `:80` admin, and `:8081` public enrolment when
`ENROLL_DB` is set. Nothing is exposed through Fly's proxy (`fly.toml` has no `[[services]]`).

A bridge is an entry host (xray/Reality front plus bridge agent) for clients on restrictive networks. For compatibility the code, the `ingresses` JSON key, the `tag:ingress` headscale tag, the `AGENT_*` variables, the `/enroll/ingresses` endpoint paths and the Ansible role/service names keep the historical name `ingress`.

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
| `ENROLL_ALLOWED_HOSTS` | optional for the core, **required** by the policy module (`POLICY_DB`) and the tasks module (on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set); comma-separated Host values the admin listener accepts (anything else gets 400); set it to block DNS rebinding. `*` allows any host (those two modules refuse it) and `*.example.com` any host ending in `.example.com` (list `example.com` too for the bare name), as in the Python app |
| `ENROLL_DB` | path of the SQLite database; enables enrolment. Put it on the state volume: `/var/lib/tailscale/enroll.db` |
| `ENROLL_REDIS` | `1` keeps the enrolment data in Redis instead of SQLite: no volume needed (see "Enrolment in Redis"). Needs `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `ENROLL_SECRET_KEY`; `ENROLL_DB` is then not used |
| `ENROLL_REDIS_KEY` | key prefix of the enrolment data, `<app>:<name>`. Default `<app>:enroll` |
| `ENROLL_IMPORT_FROM` | with `ENROLL_REDIS=1`: path of an old enrolment SQLite file, copied into Redis once when Redis holds no enrolment data (the file is opened read-only and left untouched) |
| `ENROLL_CONFIG` | enrolment bridge description, for example `/srv/app/enroll.json` (required with `ENROLL_DB`; shape under "Enrolment") |
| `ENROLL_SECRET_KEY` | secret, at least 16 bytes; derives subscription secrets (required with `ENROLL_DB`) |
| `AGENT_URL`, `AGENT_TOKEN` | address (dialled through the tailnet) and bearer token of the first bridge's agent. Required with `ENROLL_DB` only for the old single-host `enroll.json`; in the list shape they are the optional fallback of the FIRST entry (no `agent_url` / no `AGENT_TOKEN_<NAME>`) |
| `AGENT_TOKEN_<NAME>` | one bearer token per bridge: `<NAME>` is the bridge name upper-cased with `-` as `_` (`ingress-2` reads `AGENT_TOKEN_INGRESS_2`). An entry's `token_env` names another variable instead |
| `POLICY_DB` | path of the SQLite change history; enables the policy console (`/policy`). On the state volume: `/var/lib/tailscale/policy.db`. Needs `HEADSCALE_LOGIN_SERVER` and `HEADSCALE_API_KEY` |
| `POLICY_ADMIN_USERS` | comma-separated tailnet logins allowed to use the console (required with `POLICY_DB`); tagged nodes are never allowed |
| `POLICY_REVERT_SECONDS` | seconds to confirm a change before it is reverted automatically, 30 to 900, default 120 |
| `POLICY_MAX_BYTES` | largest policy accepted, 1024 to 4194304, default 262144 |
| `TASKS_*` | the scheduled-task module, on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set; variables and defaults under "Tasks module" |
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

The enrolment data can live in Redis too (`ENROLL_REDIS=1`, below). Without it the SQLite database (`ENROLL_DB`) stays
on the volume, because it is a database file and a Machine mounts one volume. The policy console history (`POLICY_DB`)
is still a SQLite file on the volume.

### Enrolment in Redis

`ENROLL_REDIS=1` replaces the SQLite file, so the machine needs no volume for enrolment. Environment: `REDIS_URL` and
`NODE_CONTROL_STATE_KEY` (the same variables as the tailscale state; the records are encrypted with that key, and the
store refuses to start without it unless `STATE_ALLOW_PLAINTEXT=1`), `ENROLL_SECRET_KEY` (unchanged: subscription
secrets are derived from it, device id and epoch), optional `ENROLL_REDIS_KEY`.

Layout under the prefix `P` (all DB 0): `P:meta` (hash; `seq` is the last device id, never reused), `P:d:<id>` (one hash
per device, no TTL: sealed name/owner/client/UUIDs plus plain `expires_at`, `revoked`, `epoch` and the activity numbers below, so revoke, extend and
rotate are single atomic commands), `P:t:<hash>` (one hash per one-time link, **with a TTL**), `P:approvals` (sealed).
A link's key expires through its TTL, and every spend also checks the stored expiry. Spending is one Lua script (link
key and device key) that writes nothing unless both exist, the device is not revoked or expired, and the link has a use left
and has not expired (a bare `HINCRBY` on an expired key would
recreate it without a TTL). Commands sent: `EVAL` (never `EVALSHA`), `HGET`, `HGETALL`, `HINCRBY`, `HSET`, `HDEL`,
`HEXISTS`; the scripts also use `EXISTS`, `HSET`, `HINCRBY` and `PEXPIRE`. Nothing scans, selects a database or
flushes. A Redis error is an error, never an empty set: the sync to the agents does not push when the store cannot be
read.

Moving an existing SQLite database: set `ENROLL_IMPORT_FROM` to the file (a start with no enrolment data in Redis copies
it once, verifies the copy and logs counts only), or run `node-control enroll-import -from <enroll.db> [-force]`. The
file is opened read-only and never modified, so the previous release can still run against it. Ids, epochs, UUIDs and
link hashes are carried over verbatim, so issued subscription secrets and one-time links keep working. `-force`
overwrites records with the same ids and never deletes others. An interrupted import leaves no device counter, so the
store still reads as empty and the import can simply be run again. A copy that fails the read-back verification removes the
counter again (when the store was empty before), so the next start imports again instead of serving a copy that never
verified.

Backup and restore:
- `node-control enroll-export [-to <file>]` writes the devices, UUIDs and approvals as JSON (stdout by default; one-time
  links are not included: Redis cannot list them without a scan).
- `node-control enroll-import -json <file|->` restores such a file into an empty Redis (`-` reads stdin, so an encrypted
  backup can be piped in without a plaintext file) and verifies the copy. Exactly one of `-from` and `-json` is given;
  the JSON is checked for shape first (unknown fields, duplicate ids, unknown modes, empty UUIDs are refused, and error
  texts never echo the file).
- `node-control enroll-export -format sqlite -to <new file>` writes a new SQLite enrolment database that the SQLite store
  (and so a release without Redis support) opens as it is, with the id counter kept ahead of every id ever issued. It
  refuses an existing file and creates it mode 0600. Use it to roll back.

The output of both exports holds device names, owners and credentials: treat it as a secret and encrypt it before it is
stored anywhere.

`scripts/enroll-backup.sh` wraps both for a Fly deployment (needs `flyctl` logged in and `jq`):

```sh
scripts/enroll-backup.sh backup  <fly-app> backup.sops.json --sops           # or: --age <recipient>
scripts/enroll-backup.sh restore <fly-app> backup.sops.json --sops [--force] # or: --age-identity <file>
```

`backup` runs `enroll-export` in the machine, keeps the output in memory (no unencrypted file is ever written), checks the JSON
shape and that it holds at least one device, encrypts it with `sops` or `age`, writes the result atomically (never over an
existing file, mode 0600), checks that no device UUID is left in the encrypted result, and prints counts only. `--plaintext`
exists for a throwaway file and must be given explicitly. With `--sops` the file name decides which `.sops.yaml` creation rule
applies, and the key names of the JSON stay readable (sops encrypts values only). Export `SOPS_AGE_KEY` (or your sops key
source) before running it. `restore` decrypts and pipes the data into `node-control enroll-import -json -` in the machine
(stdin through `fly ssh console -C`), which checks the shape, writes and verifies the copy; without `--force` a store that
already holds data refuses. Run a backup after enrolment changes; it is a manual step until an automatic one exists.
`sh scripts/test_enroll_backup.sh` tests the script against a fake `fly` (real `age` and `jq` needed, no network).

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

Setting `ENROLL_DB` turns on user enrolment for a REALITY bridge proxy. The binary then serves a second, public
enrolment listener next to the admin app. Both bind to the tailnet address only:

| port | serves | reachable by |
|---|---|---|
| 80 | admin app, including the `/enroll` admin pages | operators on the tailnet |
| 8081 | public enrolment | the bridge proxy only (restrict it in the tailnet ACL) |

Environment (secrets via Fly secrets, the rest in `fly.toml`):

- `ENROLL_DB`: path of the SQLite database: `/var/lib/tailscale/enroll.db`. It must live on a Fly volume or it is
  lost on deploy, and a Fly Machine mounts only one volume, so use the existing `tailscale_state` volume (mounted
  at `/var/lib/tailscale`, see above); do not create a second one. That volume's snapshots then also hold the
  enrolment database (device names and owners, not the subscription secrets, which are derived from
  `ENROLL_SECRET_KEY`); treat them accordingly. Back it up with `sqlite3 ... .backup` before risky changes.
- `ENROLL_REDIS=1`: store the enrolment data in Redis instead (next subsection). `ENROLL_DB` is then not used.
- `ENROLL_CONFIG`: path of a JSON file describing the bridge nodes (shape below). Keep the real file in your
  private repo and overlay it.
- `ENROLL_SECRET_KEY`: at least 16 bytes; derives the subscription secrets and the per-bridge credentials. Changing
  it invalidates both (see "Credentials per bridge").
- `AGENT_URL`, `AGENT_TOKEN`: address and bearer token of the first bridge's agent (the old single-host setup).
  Further bridges use `agent_url` in their entry (or are found through headscale) and `AGENT_TOKEN_<NAME>`.

- `ENROLL_ALLOWED_HOSTS` (optional for the core; required, and must not be a bare `*`, when the policy or tasks module
  is on): comma-separated Host header values the admin app accepts, for example
  `node-control.<tailnet-domain>,<tailnet-ip>`. Any other Host gets `400` (the same rules as Starlette's `TrustedHostMiddleware`, applied
  to the whole admin app on port 80). Set it: it stops a malicious web page from reaching the admin API through DNS
  rebinding in an operator's browser. List every name/IP you open the page with.

### `enroll.json`

One or more bridge nodes. Each entry is a bridge host with its own agent; a device gets a credential on every
active bridge and the phone profile lists them all.

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

- Top level: `ingresses` (1 to 8 entries, required), `link_host` (required with more than one bridge; with one it
  defaults to that bridge's `host`), `profile_name` (label of the imported profile, default `tunnel`). Unknown keys
  are an error.
- Per entry: `name` (`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, unique), `host` (unique DNS name), `public_key`,
  `short_id`, `uuids` (required, see below). Optional: `sni` (default `host`), `port` (default 443),
  `agent_url` (`http(s)://host:port`; without it the first entry uses `AGENT_URL` and the others are found through
  headscale as the `tag:ingress` node carrying the entry's name, agent port 9000), `token_env` (name of the token
  variable, `^[A-Z][A-Z0-9_]{0,63}$`; default `AGENT_TOKEN_<NAME>`, and for the first entry also `AGENT_TOKEN`).
  `sni`, `short_id` and `public_key` must equal the `reality-ingress` role's `reality_sni`, `reality_short_id` and
  the public half of `reality_private_key` (`xray x25519 -i <private>`); `port` must equal its `ingress_listen_port`.
- `link_host` is the host of subscription URLs and one-time links. Phones fetch `https://<link_host>/sub/<secret>`
  and `/e/<token>`, so the name must resolve to a bridge whose front serves it and proxies to node-control's
  `:8081`, with a certificate for it. Prerequisite: DNS for `link_host` and a working certificate on whichever
  bridge it points at, before the first device is enrolled. Every bridge's front must be able to serve it, so
  moving the name to another bridge is enough to fail over.
- The old single-host file (`host`, `public_key`, `short_id`, `sni`, `port` at the top level, no `ingresses`) still
  loads as one bridge named `default` with `"uuids": "base"`, and uses `AGENT_TOKEN`. Mixing the two shapes is an
  error.

### Credentials per bridge (`uuids`)

`uuids` is required on every bridge in the list shape; there is no default, because a value that depended on
the other entries would change meaning when one is added or removed.

- `"base"`: the device's stored UUIDs are the credential on this bridge. At most one bridge may use it, the one that
  exists today.
- `"derived"`: the credential is computed per bridge and device mode with HMAC-SHA256 keyed by
  `ENROLL_SECRET_KEY`, so a credential taken from one bridge is useless on the others, and a compromised bridge
  cannot compute any other bridge's values.

Migration from the single-host file: write the list shape with the existing bridge as an entry with
`"uuids": "base"` (its name does not matter for `base`, `default` or any other) and every new bridge as `"derived"`.
Existing phones keep working untouched, because their credential on the old bridge did not change.

Consequences to plan for:

- The bridge NAME is part of a derived credential. Renaming a `derived` bridge rotates its credentials: phones need a
  new profile. Reordering entries does not change any credential, but the FIRST entry owns the `AGENT_URL` /
  `AGENT_TOKEN` fallback and the link view (key, short id and sni used for import links), so moving another entry to
  the top changes which agent those variables reach.
- Rotating `ENROLL_SECRET_KEY` invalidates the subscription secrets AND every derived credential. Re-publish
  and re-enrol the profiles. `base` credentials survive.
- Removing an entry from the file stops syncing to it: that bridge keeps its last credential set, and a later
  revocation does not reach it. Take the host down or wipe its agent first.

### Sync with the bridge host

Every create/revoke/extend pushes the full credential set to the agent; a reconcile loop retries every 5 minutes.

Activity (no connection log, no client addresses): each device row shows **Enrolled** (when a one-time link of the device was
last spent, and how often) and **Last active** (when its traffic last grew). Spending a link marks the device in the same
step as the spend (one SQLite transaction; one script on Redis). Last active is derived from the traffic totals the agents
already report: node-control looks every 5 minutes (and at each page load) and records the time when a device's total grew
since the previous look. The counters reset when xray restarts (the agent restarts it whenever the credential set changes),
so a total that fell but is above zero also counts as activity since the reset. The first look at a device only seeds the
baseline, and a look at which a bridge did not answer is skipped (its missing counters would read as a reset). The
resolution is therefore about 5 minutes, and a device that was active only while node-control was down shows nothing for that
time. `GET /enroll/devices` rows carry `enrolled_at`, `enrol_count` and `last_active` (unix seconds, 0 = never). SQLite databases
gain the three columns on open; Redis device records without them read as 0. JSON and SQLite exports and imports carry them.

Which buttons a row offers depends on its state, so nothing useless is shown: a revoked device only has Delete; an expired
one has Extend and Revoke; an active one that has never been enrolled has New link, Extend and Revoke; an active one that
is already enrolled has Extend and Revoke (it has its profile). `POST /enroll/devices/{id}/links` answers `409` for a revoked
or expired device, because a link for it would be created and never work.

Deleting: `DELETE /enroll/devices/{id}` (JSON content type, like the other changes) removes a **revoked** device with its
UUIDs and links for good. An active or merely expired device is refused with `409` ("revoke the device before deleting
it"), an unknown id with `404`. The id is never issued again. Nothing is pushed to an agent (only active devices are ever
sent). The `/enroll` page hides revoked devices by default ("Show revoked devices (N)", remembered in the browser) and
offers only a Delete button on a revoked row. On Redis the delete is one guarded script (`DEL` on the device key only when
it exists and is revoked); links of the device expire on their own TTL and can no longer be spent.
A failed push is logged (exception class, and the HTTP status for HTTP failures; never payloads or credentials) and
classified, per bridge: `unreachable` (no connection), `unauthorized` (401/403, `AGENT_TOKEN` mismatch), `rejected` (other
non-2xx, typically 503: xray refused the generated config), `conflict` (409), `error` (unexpected exception in the
loop). `GET /enroll/devices` returns `{"devices": [...], "sync": {"pending": bool, "state": "...", "since": <unix>|null}}`;
create/revoke/extend responses carry `sync_pending` and `sync`. While `pending`, the `/enroll` page shows a banner
"bridge host out of sync since <time>: <state>": until it clears, revocations and extensions are not in effect on
the bridge host. A store with no devices at all is never pushed (state `empty`): the agent would refuse an
unconfirmed empty set, and an empty store must never wipe the agent.

Every bridge has its own sync state and gets its own credential set (the derived ones for a `derived` bridge). A
failure on one bridge never fails the operation: a create, revoke or extend is recorded first and answers success,
the response and the page show the bridge that is behind (`sync` aggregates, `GET /enroll/ingresses` has one row per
bridge), and the reconcile loop retries it. Revoking therefore never blocks on an unreachable bridge; until that
bridge is back, the revoked credential still works there. The banner and the per-bridge rows tell which one.
Traffic figures are merged per device across the bridges; when a bridge with a pending sync answered nothing,
`stats_partial` is `true` and the page marks the column `Traffic*`: the numbers are a lower bound.

### Discovered bridges, approval and slow agents

Nodes tagged `tag:ingress` in headscale that are not listed in `enroll.json` are discovered (needs `link_host` in the
file and `HEADSCALE_API_KEY`; the discovery line on the page says why it is off) and shown on the `/enroll` page. Each
one is asked for its public parameters (`GET /v1/info` on its agent, bearer token `AGENT_TOKEN_<NAME>`, `<NAME>` the
node name). A discovered bridge is used only after the operator approves it; the approval pins the fingerprint of its
published parameters AND the headscale node id that published them. Discovered bridges always use derived
credentials. Approvals live in the enrolment database. States of a row: `configured` (listed in `enroll.json`,
approved by definition), `pending` (never approved), `approved`, `changed`, `unreachable`, `no token`, `invalid`.

Adding a bridge, step by step:

1. Deploy the host with the `reality-ingress` and `ingress-agent` roles; give its agent its own token and set the
   `INGRESS_*` values so `/v1/info` answers. Its front must serve `link_host` (see "Compromised bridge").
2. Register the node in headscale with `tag:ingress` and a name that fits `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`
   (this becomes the bridge name). Then pin the name with `headscale nodes rename <id> <name>`, using a name that the
   machine's hostname does not produce (otherwise headscale follows a hostname change). `headscale nodes list` shows
   the id, addresses and names.
3. Set the token as a secret named `AGENT_TOKEN_<NAME>` and redeploy node-control (or use an `enroll.json` entry
   instead, see above, which needs no approval).
4. Open `/enroll`. The bridge appears as `pending` with its host, the node id, tailnet address and names, a short
   prefix of its public key and short id, a short prefix of its machine key and a fingerprint. Compare the node id,
   address, name and machine key with `headscale nodes list`: an impostor holding the name can publish the real bridge's public values, so the node
   itself must be the one you registered. The agent does not report a fingerprint: it is node-control's SHA-256 over the canonical JSON of
   `name`, `host`, `port`, `sni`, `public_key` and `short_id`. Compare the host, public key and short id that
   `GET /v1/info` on the agent reports (over a channel you trust) with what the page shows, then press Approve. The
   API is `POST /enroll/ingresses/<name>/approve` with `{"fingerprint": "<shown fingerprint>", "node_id": <shown node id>}`;
   a fingerprint that no longer matches the parameters the bridge publishes at that moment, or a node id other than
   the node that publishes them, is refused (409). `POST
   /enroll/ingresses/<name>/unapprove` takes it out again. Both POSTs need `Content-Type: application/json`.
5. Approval pushes the credential set at once; the row shows its sync state. Phones pick the new bridge up when they
   refresh their subscription; a profile imported by a one-time link needs the person to refresh it.

- A `changed` bridge (its parameters differ, or another node id publishes them) is frozen: it is out of profiles
  and no longer synced, so it keeps its old credential set until it is approved again or un-approved. A revocation
  does not reach it meanwhile.
- Names in headscale are NOT proof of who owns a node. The given name starts as the hostname the machine reports
  and headscale re-derives it whenever that hostname changes, as long as the given name still follows it (equals the
  hostname, or the hostname plus `-<digits>`). Only a name set with `headscale nodes rename <id> <name>` to something
  the hostname does not produce stays put. So:
  - **Set an explicit `agent_url` (the node's tailnet IP, for example `http://100.64.0.12:9000`) on every configured
    bridge.** It removes the headscale lookup altogether: the token goes only to that address, which no machine
    can influence. (`AGENT_URL` does the same for the first entry.) The name lookup is only the fallback, and it
    is the weaker setting.
  - For a CONFIGURED bridge without `agent_url`, the lookup accepts a tagged node only when its given name does not
    follow its hostname, by headscale's own rule (the given name equals what headscale derives from the hostname,
    optionally followed by `-<number>`; hostnames that sanitise to nothing are refused too). Run
    `headscale nodes rename <id> <name>` on the bridge node first (the log says so when a lookup is refused). When
    several nodes share the name the lookup fails and nothing is sent to any of them.
  - A DISCOVERED bridge is identified by the node id AND the machine key it was approved on, not by its name: the
    agent is found by that pair, and no other node is ever sent its token or credentials. The id alone is not enough
    (ids can repeat after a headscale database is rebuilt or restored); the machine key is what a node cannot
    choose. If the node list does not report machine keys, a bridge cannot be approved. Approvals made before
    machine keys were pinned show `changed` ("approve again") until you approve them once more. If that node disappears, the row turns `invalid`
    ("the approved node is not present") and nothing is contacted until you remove the approval; a different node that
    now carries the name can then be reviewed and approved.
- Before approval, the token variable `AGENT_TOKEN_<NAME>` is already sent (as the bearer of `GET /v1/info`) to a
  tagged node whose given name matches it. Pinning the node id after approval reduces this exposure but cannot remove
  it: anyone who can register a tagged node with that name (and, for a host with no rename, a matching hostname)
  receives that one token. Do not set the variable before you expect the node.
- A create, revoke, extend or approval answers after at most 3 seconds. If an agent has not answered by then, its row
  reads `pending` with state `unknown` until the push ends; the change itself is already recorded and never fails.

### Compromised bridge

Threat model, in short: a root attacker on one bridge host sees client source addresses and traffic per credential,
its own Reality private key, and the credentials pushed to it, and controls its own xray and agent (a revocation
does not bind it). Because TLS ends on the bridge's front, then plain HTTP goes over the tailnet to node-control, it
also sees every one-time link token and subscription secret that passes through it. It cannot reach node-control's
admin port or the other bridges' agents (restrict both in the tailnet ACL: the agent port only from node-control,
`:8081` only from the bridge hosts), nor the tailnet through xray.

What the design limits:

- Credentials are per bridge (`derived`), so what is stolen from a bridge's agent or xray is valid on that
  bridge only. This holds for a bridge that never served `link_host`. A bridge whose front serves `link_host`
  terminates TLS for profiles and subscription secrets: it sees complete profiles, with every bridge's credentials
  (the `base` ones included), and any host tagged `tag:ingress` may fetch `:8081/sub/` with a secret it harvested. If
  such a bridge is compromised, rotate the subscription secrets and `ENROLL_SECRET_KEY` (this also changes every
  derived credential) and re-enrol the devices that held `base` credentials. The `base` bridge holds the stored
  credentials; keep it the one you trust most or migrate it to a derived entry in time.
- A discovered bridge is never used before approval, and the approval pins the fingerprint and the node id. A
  changed host is frozen out of profiles and sync until approved again.
- Each bridge is asked with its own token and is pushed only its own credential set (see the condition above for a bridge that serves `link_host`).

What it does not limit: a compromised bridge that serves `link_host` can hand phones a modified profile (clients do
not verify a signature) and read the tokens and secrets that pass through. The canary only detects this.

Detection: `scripts/profile-canary.sh <tailnet-base-url> <link-host> [<canary-sub-secret> | -]` fetches a dedicated canary
device's profile directly (`<tailnet-base-url>:8081/sub/<secret>`) and through `https://<link-host>/sub/<secret>`,
compares SHA-256 of the two bodies and prints the two short hashes and `OK` or `MISMATCH`, never the body or the
secret (exit 0 identical, 1 different, 2 usage or fetch error; each side must answer exactly 200 with a non-empty
body, a redirect or an empty answer is an error). Create a device just for it and run the script from cron or CI on a
machine on the tailnet; the secret is the canary device's `<digits>.<token>`. Give it as the environment variable
`CANARY_SECRET` or as `-` (one line on stdin): an argument shows in `ps`, and `sh -x` would print it. A false
`MISMATCH` is possible if the approved set of bridges changes between the two fetches; run it once more before
acting. It needs `curl`
and `sha256sum` or `shasum`; `sh scripts/test_profile_canary.sh` tests it against local fakes. It only catches an
bridge that serves the canary a different profile than everybody else; one that targets other phones, that only
watches, or that recognises the canary by its fixed `User-Agent` (`profile-canary`) or its source address and serves it
the honest profile, passes.

Structural fixes (not built): serve `link_host` from a host that is not on the data path of any bridge, or pass TLS
through the front to a certificate held by node-control's side, so no bridge terminates it.

If a bridge is compromised, follow the site's incident runbook (`ingress-compromise`, kept in the private site
repository that runs this deployment): cut the host off, rebuild it, rotate what it held and re-enrol affected devices.

### What the bridge host sees

The one-time link (`/e/<token>`), its redirect and the subscription (`/sub/<secret>`) all pass through the bridge
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
The bridge hostname (the VLESS outbounds' `server`) is the exception: a DNS rule `outbound: any` resolves it with the
phone's local resolver (`local` server, no detour), since resolving it through `select` would need the outbound that
needs the lookup. All other lookups go through the selected outbound. That this works, and that only the bridge
hostname reaches the local resolver, is verified only in the phone test:
in `exit` mode a site that is unreachable from the client's own network must open, and a DNS leak test must show only the exit's resolver.

Differences from the Python app this replaced: the agent is dialled through the embedded tailnet node, and the
database is closed only after both listeners have drained on shutdown. A misconfigured enrolment (a missing variable,
an unreadable or invalid `ENROLL_CONFIG`) stops startup with a message naming the problem, never a secret.

## Navigation bar

Every admin page shows a small top bar that lists the modules available in this deployment, with the current one
highlighted. Today that is Nodes (`/`) and, when the module is on, Bridge (`/enroll`, with `ENROLL_DB`), Policy (`/policy`) and Tasks (`/tasks`). The bar appears only
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
only while headscale still serves that change's policy. "Revert now" in the pending banner undoes the pending change at once instead of
waiting for its deadline (the history "Revert" buttons restore an older policy as a new change, so they are disabled while one is pending). If the automatic revert keeps failing, the page shows a warning;
Confirm is then the way out, keeping the change.
The console adds a "Policy" entry to the top bar. Its Content-Security-Policy allows inline styles (the bar is one) but scripts only from the
page's own files.

## Funnel module (optional)

Setting `FUNNEL_DB` makes node-control the single admin surface of the `funnel-ingress` app (`apps/fly/funnel-ingress`):
the ingress has no admin page of its own, it **pulls** its configuration from here and **reports** what it observes.
Without `FUNNEL_DB` nothing is registered. The module needs the policy console (`POLICY_DB`), because every ACL change it
makes goes through the console's confirm-or-revert, and the same `ENROLL_ALLOWED_HOSTS` rule (no bare `*`).

What the Funnel page (`/funnel`, users in `FUNNEL_ADMIN_USERS` only, tagged nodes never) does:

- **Namespaces** (a name plus allowed port ranges). Creating, changing the ports of, or deleting one regenerates its ACL
  entries (`tagOwners` for `<prefix><ns>` and one `ingress tag -> <prefix><ns>:<ports>` rule) and submits them as one policy
  change. The namespace is `pending` until that change is confirmed, `active` after, `failed` if it was reverted; the ingress is
  only ever given active namespaces. A port change keeps serving the old ports until confirmed. Delete is refused while the
  namespace has manual routes or the ingress still reports registered routes in it. If the live policy has comments or trailing
  commas the change is refused (edit it in the policy console instead): nothing is dropped silently.
- **Publisher keys**: minted through the headscale API for the user `FUNNEL_KEY_USER` with the tags fixed by the server
  (`FUNNEL_TAG` plus `<prefix><ns>`; other request fields are ignored), shown once and never stored, logged or audited. Listing
  shows keys carrying the namespace tag; expiring only works on publisher keys.
- **Manual routes**: the namespace must be active, the target a headscale node (name or tailnet address) carrying the namespace
  tag, the port inside the namespace's ports.
- **Blocked names**: the ingress removes a registered route for the name and refuses re-registration.
- The latest **ingress report** with its age (stale after 90 s), a per-namespace DNS and certificate checklist, and an audit log.

The ingress reaches two machine endpoints on the admin listener, `GET /ingress/v1/config` (with `ETag`, version = hash of the
content) and `POST /ingress/v1/state` (1 MiB cap), and only as a node carrying `FUNNEL_INGRESS_TAG`; a human is refused there
and a tagged node on `/funnel/*`.

| variable | purpose |
|---|---|
| `FUNNEL_DB` | path of the SQLite database; enables the module. On the state volume, for example `/var/lib/tailscale/funnel.db` |
| `FUNNEL_PUBLIC_DOMAIN` | lower-case DNS name the namespaces live under (`<service>.<ns>.<domain>`); not under the headscale base domain |
| `FUNNEL_INGRESS_HOSTNAME` | optional name the wildcard record points at, for example `<app>.fly.dev` (shown in the checklist) |
| `FUNNEL_TAG`, `FUNNEL_NS_TAG_PREFIX`, `FUNNEL_INGRESS_TAG` | tags: publishers, namespace prefix, the ingress node; defaults `tag:funnel`, `tag:ns-`, `tag:funnel-ingress` |
| `FUNNEL_TAG_OWNERS` | owners written into the generated `tagOwners` entries (comma-separated users or groups) |
| `FUNNEL_KEY_USER` | headscale user NAME that owns publisher keys (resolved to an id when minting) |
| `FUNNEL_ADMIN_USERS` | tailnet logins allowed to use the page and API (required) |
| `FUNNEL_RESERVED_PORTS` | ports the ingress uses itself, default `8080,9100`; no namespace may allow them (the ingress would reject the whole configuration) |

Headscale does not check tag ownership when a key is created or used, so `FUNNEL_TAG_OWNERS` does not gate minting: the fixed
tags and the user gate are the controls.

## Tasks module (optional)

Scheduled checks that drive a browser script on one of your nodes, with retries, alerts and observability. A task
names a script, a node (or a set of tags), a schedule and what to do with the result. node-control picks an eligible
node, hands the script and its secrets to the [`task-runner`](../../task-runner/README.md) on that node, follows the run
to its end, and applies the retry, alert and notification rules. The script is generic: it opens pages, reads a value
and returns a small JSON object. Nothing about what is checked lives in this repository.

```
 operator ──tasks.sh / /tasks page──▶ node-control (tasks module) ──── HGET/HSET ────▶ Redis
 (tailnet user)                       │  config, scripts, secrets,                    hash node-control:tasks
                                      │  state and run history, scheduler,            (values encrypted)
                                      │  alerts, metrics, heartbeat
                                      │  start / poll / cancel, over the tailnet
                                      ▼
                                   node (tailnet) ── task-runner :9100 ── rootless container: Chromium + script
```

The module lives in `internal/tasks`. The admin API and page are served on the admin listener (`:80`), gated by the
caller's tailnet identity. Redis being down never stops node-control: the tasks routes answer `503` until the store
opens (it is retried in the background, after 30 s, then 1, 2 and every 5 minutes), while scheduling keeps running from
memory once it is open.

### Activation and environment

The module is on only when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are all set; otherwise it logs
`tasks module inactive: not set: ...` naming the missing ones and stays off (no routes, no top-bar entry). Once on,
a bad value stops startup with a message that names the variable and never a secret. `REDIS_URL` and
`NODE_CONTROL_STATE_KEY` are the same ones as for the Tailscale state (see above); the tasks data is always encrypted,
`STATE_ALLOW_PLAINTEXT` does not apply to it.

| variable | default | purpose |
|---|---|---|
| `REDIS_URL` | | secret; the shared Redis (see the table above) |
| `NODE_CONTROL_STATE_KEY` | | secret; encrypts every stored value (AES-256-GCM, each value bound to its Redis field name) |
| `TASKS_RUNNER_TOKEN` | | secret; bearer token the node runners accept (their `RUNNER_TOKEN`) |
| `TASKS_ADMIN_USERS` | | comma-separated tailnet logins allowed to use the page and API. Required once the module is on; tagged nodes are never allowed |
| `TASKS_RUNNER_TAG` | `tag:task-runner` | headscale tag a node must carry to run any task (matches `^tag:[a-z0-9][a-z0-9-]{0,62}$`). Nodes without it are never probed and never receive the runner token or a task's secrets |
| `TASKS_RUNNER_PORT` | `9100` | port of the runner on the nodes, 1 to 65535 |
| `TASKS_NTFY_URL` | `https://ntfy.sh` | base URL of the ntfy server used for alerts and failure messages |
| `TASKS_POLL_SECONDS` | `5` | how often a running attempt asks its runner for the status, 1 to 60 |
| `TASKS_METRICS_ADDR` | unset | `host:port` of the metrics listener, for example `:9091` (host may be empty, port numeric). Unset: no listener |
| `TASKS_HEARTBEAT_SECRET` | unset | NAME of a stored secret (letters, digits, `_`, at most 64) whose value is a heartbeat ping URL. Unset: no heartbeat |

The module requires the admin listener's Host guard, like the policy console: once it is on, `ENROLL_ALLOWED_HOSTS`
must list the host names the admin listener answers to, and an unset, empty or `*` value stops startup. The guard
protects the page and API from DNS rebinding.

Rollout: give every runner node the runner tag in headscale and write the tailnet ACL rule that opens the runner port
with that tag as the destination. A node without the tag is ignored even when a task names it.

The tag is what node-control trusts with the runner token and the task secrets. It is trusted because the policy's
`tagOwners` decides who may have it when a node registers (`tailscale up --advertise-tags` is checked against it), and
on 0.28 and later only an admin changes a node's tags afterwards (`headscale nodes tag` or the API). How headscale stores tags depends on
the version, and node-control reads both forms:

- headscale 0.28 and later (unified model): a node has one `tags` list. Tags and user ownership exclude each other:
  tagging a user-owned (tag-less) node removes its user ownership, and only re-authenticating it with empty tags undoes
  that.
- Older headscale (legacy model): forced tags, which an admin sets, plus valid tags, which a node can request at any
  time with `--advertise-tags` and gets when `tagOwners` lets its user own them.

So the trust boundary for the runner tag is the policy's `tagOwners` entry: list ONLY the admin/service identity as
owner of the runner tag, never human users whose devices are not runners. The safest way to tag a node is as an admin
(`headscale nodes tag`, `scripts/headscale-tag-node.sh` below, or a pre-auth key created with that tag as its ACL tag);
a node that merely advertises the tag gets it only if the policy lets its user own it.

With the helper (needs `HEADSCALE_URL` and `HEADSCALE_API_KEY`, or `HEADSCALE_API_KEY_CMD` printing the key): first run
`scripts/headscale-tag-node.sh show --node <given-name>` and read the model it prints (`unified` or `legacy`), then
`scripts/headscale-tag-node.sh add --node <given-name> --tag tag:task-runner` without `--yes` to see the plan
(`tags: [...] -> [...]`, or `forced: [...] -> [...]` on a legacy server). The same command with `--yes` applies it,
keeping the node's other tags (the API replaces the whole set: `tags` on the unified model, the forced tags on the
legacy one), and checks the result by reading the node again. On the unified model, tagging a node that has no tags yet
removes its user ownership, so the helper also requires `--allow-ownership-change` there. Run `show` again afterwards:
the runner tag is listed under `tags` (unified) or `forced` (legacy).

A given name follows the node's reported hostname until it is set with `headscale nodes rename` (see "Names in
headscale" above), so rename the runner nodes if `node` targets should not depend on what a runner reports.

### Task configuration

The configuration is one JSON object, `{"tasks": {"<id>": {...}}}`. It is decoded strictly: unknown fields, trailing
data, a duplicate task id and a bad value are all errors, and every problem is reported with its task and field. A task
id matches `^[a-z0-9][a-z0-9._-]{0,63}$` (`config` and `secrets` are reserved: they would clash with the API paths;
`scheduler` is reserved too: it is the heartbeat's label in metrics and logs).
Durations are Go duration strings (`"90s"`, `"12h"`); a bare number is refused because its unit would be a guess.

```json
{
  "tasks": {
    "check-balance": {
      "script": "check-balance.js",
      "params": { "account_url": "https://example.com/account" },
      "target": { "tags": ["tag:region-xx"], "online": true },
      "schedule": { "every": "12h", "jitter": "30m" },
      "timeout": "3m",
      "retry": { "attempts": 3, "backoff": ["2m", "10m"] },
      "secrets": ["LOGIN", "PASSWORD"],
      "healthcheck": "CHECK_BALANCE_PING",
      "alert": {
        "when": { "field": "balance", "op": "<", "value": 10 },
        "cooldown": "24h",
        "notify": {
          "topic": "NTFY_TOPIC",
          "priority": "high",
          "title": "{task}: balance is low",
          "message": "Balance is {balance}"
        }
      },
      "on_failure": {
        "cooldown": "24h",
        "notify": {
          "topic": "NTFY_TOPIC",
          "title": "{task} failed",
          "message": "{reason}"
        }
      }
    }
  }
}
```

A complete placeholder file is in
[`examples/private-repo/fly/node-control/tasks/tasks.example.json`](../../../examples/private-repo/fly/node-control/tasks/tasks.example.json).

| field | required | default and rules |
|---|---|---|
| `script` | yes | file name of a stored script: no `/`, `\` or `..`. A push also needs the name to match `^[A-Za-z0-9._-]{1,64}$` |
| `params` | no | JSON object handed to the script as `params` |
| `target` | yes | either `node` (the node's headscale **given name**, exactly; the host name a node reports is never matched, since any node can claim any host name) or `tags` (a node must carry all of them), never both. Two nodes with the same given name make the target ambiguous: the attempt fails with `ambiguous target` and nothing is contacted. Either way only nodes with the runner tag (`TASKS_RUNNER_TAG`) are candidates. `online: true` skips nodes headscale reports offline |
| `schedule.every` | yes | at least `1m` |
| `schedule.jitter` | no | `0s`. The next run is `now + every ± jitter` (uniform), never sooner than `every / 2` |
| `timeout` | no | `3m`; from `1s` to `30m` (the runner counts whole seconds). Enforced by the runner |
| `retry.attempts` | no | `1`; 1 to 10 attempts per sequence |
| `retry.backoff` | with attempts > 1 | list of positive durations; entry *n* is the wait after failed attempt *n*, the last entry repeats |
| `secrets` | no | names of stored secrets passed to the script as `secrets.<NAME>`. The runner hands secrets and params to the container in environment variables, so a push is refused when the JSON of a task's secret values plus its params JSON exceeds 100000 bytes |
| `healthcheck` | no | NAME of a secret holding a healthchecks-style ping URL |
| `alert.when` | with `alert` | `field` (a numeric result field), `op` (one of `<` `<=` `>` `>=` `==` `!=`) and `value` (number) |
| `alert.cooldown` | no | `24h`; must be positive |
| `alert.notify`, `on_failure.notify` | with the block | `topic` (a secret NAME: the ntfy topic is never in the config; `ntfy.sh` is a public service where the topic name is the only access control, so store a long random topic as the secret), `priority` (`min`, `low`, `high`, `max`; anything else is ntfy's default), `title`, `message` |
| `on_failure.cooldown` | no | `24h`; must be positive |

`{name}` in a title or message is replaced by the scalar result field of that name (alert) or by `task` and `reason`
(failure); an unknown name stays as written so a typo is visible. `{task}` is also available in alert text. A
placeholder with a colon is refused.

Which node runs a task: the nodes with the runner tag that match `target` (and are online when `online` is set), whose
runner answers its health check as reachable and idle. Among them the node that last ran the task successfully is preferred, then the least
busy one, then the lowest name. A candidate node must also have a tailnet IPv4 address. None eligible makes the attempt fail with `no eligible node`.
A runner that refuses the token (`401`) counts as unhealthy; node-control logs `runner rejected the token` with the
node's name once per process start.

### Script contract

A script is a CommonJS module exporting `run` (`module.exports = { run }` or `module.exports = fn`). It is called as
`run({ page, params, secrets, log })`: `page` is a Playwright page of a fresh headless Chromium context (nothing is
shared between runs), `params` and `secrets` are objects, and `log(msg)` writes a line to the run's log tail. That tail is visible only
from the runner's own `GET /v1/runs/{id}` (`log_tail`); node-control neither stores nor shows it. It returns a
small JSON object. The runner refuses a result that is not an object or is larger than 4096 bytes (the run fails and says
why). Throw, or return a rejected promise, to fail the run. Details and the isolation of the container are in the
task-runner README. A commented example is in
[`examples/private-repo/fly/node-control/tasks/scripts/check-balance.example.js`](../../../examples/private-repo/fly/node-control/tasks/scripts/check-balance.example.js).

Secret values are scrubbed (replaced by `***`) from the run's reason and from the result before they are stored or shown.
A script should still never return a secret.

### HTTP API

Every route is on the admin listener and needs a tailnet user from `TASKS_ADMIN_USERS`: anyone else, and every tagged
node, gets `403` with an empty body. While the store is not open each route except the page and its static files answers
`503 {"error":"degraded"}`. Errors have the shape `{"error":"..."}`; a push that fails validation adds
`"problems": [...]`. Every `POST` and `PUT` needs `Content-Type: application/json`, even `POST /tasks/{id}/run`, which has no body;
otherwise the answer is `422 {"detail":"Content-Type must be application/json"}`. For example:
`curl -X POST -H 'Content-Type: application/json' -d '{}' "$NODE_CONTROL_URL/tasks/check-balance/run"`. Answers are `Cache-Control: no-store`. Times are RFC 3339 UTC.

| method and path | answer |
|---|---|
| `GET /tasks` | the HTML page (static files at `/tasks/static/tasks.css` and `tasks.js`), served even while degraded |
| `GET /tasks.json` | list of task views, sorted by id (`[]` before the first config) |
| `GET /tasks/{id}` | one task view; `404 {"error":"unknown task"}` |
| `POST /tasks/{id}/run` | start a run now: `202 {"run_id":"..."}`; `409` already running; `404` unknown task; `503` shutting down |
| `GET /tasks/{id}/runs` | the newest 50 runs of the task, newest first, as run views; `404` unknown task |
| `GET /runs/{id}` | one run view, searched among the executing attempts and every task's history; `404 {"error":"unknown run"}` |
| `DELETE /runs/{id}` | cancel an executing run: `204`; `409` when the run is recorded as running or queued but is not executing in this process; `404` unknown or finished |
| `GET /tasks/config` | `{"config": {...}, "scripts": {"name": "text"}}`: the live config (defaults applied) and the scripts it references; `404 {"error":"no config"}` before the first push |
| `PUT /tasks/config` | body `{"config": {...}, "scripts": {"name": "text"}}`; add `?dry_run=1` (or `true`) to validate only. Answer `200 {"tasks": {added, changed, removed}, "scripts": {added, changed, removed}, "dry_run": bool}`, plus `"pending_write": true` when Redis could not be written yet. `413` for a body over 1 MiB or a script over 256 KiB; `422` for an invalid body, a script name outside `^[A-Za-z0-9._-]{1,64}$`, an invalid config (`problems`), a reserved task id, a task naming a secret that is not stored (**push secrets first**), a task whose secrets and params exceed the runner's 100000-byte limit, or a script that is neither in the body nor stored |
| `GET /tasks/secrets` | `[{"name": "...", "set": true}]`; values are never returned |
| `PUT /tasks/secrets` | body `{"NAME": "value", ...}`: at most 64 names (`^[A-Za-z0-9_]{1,64}$`), each value 1 to 4096 bytes, body at most 64 KiB. `200 {"stored": [names]}` (plus `pending_write`); `413`; `422` with a message that never echoes a value or a bad name |

Task view: `id`, `script`, `target`, `every`, `running` (bool), `running_run` (a run view or null), `last_success`,
`last_result` (result of the newest successful run), `streak`, `attempt` (above 0 while a retry is pending), `next_run`
(the retry time during a retry sequence), `stale`. A task is stale when it is overdue for a success: its last success is
older than twice `every`, or it never succeeded and either failed for real or its first run is more than a minute late
(a cancel or a run cut short by a shutdown or restart does not count as a failure here).

Run view: `id`, `task`, `node`, `attempt`, `state` (`queued`, `running`, `succeeded`, `failed`, `lost`, `cancelled`),
`reason`, `started`, `ended`, `duration_ms` (null until the run ended), `result`, `script_sha` (SHA-256 of the script
that ran) and `notify_outcome` (for example `alert=sent ping=sent`; `suppressed` when a cooldown held the message back).
Where a run lives on its node is never exposed. A push replaces the config and the scripts it references as a whole;
nothing deletes a stored script or secret (they stay, unreferenced, until removed from Redis by hand). Consequences:

- A raw API caller (not `tasks.sh`, which always sends every referenced script) that PUTs a config naming a script
  without its text in `scripts` silently reuses the stored text of that name, even an old orphan.
- Removing a task id keeps its state and history in Redis; `GET /tasks.json`, `GET /tasks/{id}`,
  `GET /tasks/{id}/runs` and `GET /runs/{id}` only look at configured tasks, so they no longer show them. Re-adding the
  same id resumes that state: its streak, a pending retry, its next run and the alert and failure-message cooldowns.
  Use a new id for a fresh start.

### Storage

Everything is one Redis hash, `node-control:tasks`, on the shared Redis. Each value is encrypted with
`NODE_CONTROL_STATE_KEY` and bound to its field name, so a value copied to another field does not decrypt. Fields:
`config`, `script:<name>`, `secret:<name>`, `secrets-index`, `state:<task>` and `runs:<task>`.

The store reads every field once when it opens and serves everything from memory afterwards. It only writes (one `HSET`
per changed field) and reads with `HGET` at open: nothing polls Redis, which matters on a Redis billed per command.
Timestamps that drive behaviour (next run, retry time, last success, the alert and failure-message cooldowns) are stored
as absolute times, so a restart resumes exactly where it was and an expired cooldown needs no cleanup. When a write
fails, the value stays in memory and is retried by the next write (the API says `pending_write`); the scheduler keeps going
with Redis down.

A stored value the module cannot use (it does not decrypt, for example after `NODE_CONTROL_STATE_KEY` was changed, or the
config no longer passes validation, or a state or history field does not decode) does not keep the module closed: the
field is logged by name (`task store field unusable, ...`, never its value) and treated as absent. A bad config reads as
no config, an unreadable secret is not listed, a bad state or history starts empty; `tasks.sh push` (with its secrets)
repairs it. Only a failure to reach Redis keeps the routes at `503`.

History is bounded: the newest 200 runs per task, and the whole `runs:<task>` field is kept under 256 KiB by dropping
the oldest runs. A stored reason is cut at 512 bytes and a result larger than 8 KiB is stored as `{"truncated": true}`.

### Retry, failure and alert semantics

A *sequence* is the attempts one scheduled (or manual) run makes. An attempt that fails with attempts left stores the
attempt number and its due time and returns: nothing sleeps, the scheduler wakes at that time, and a restart resumes the
backoff. Only the last failed attempt of a sequence counts: it increments `streak`, sends the failure message (when
`on_failure` is set; at most once per its cooldown) and the failure ping. A success resets `streak` and schedules the next
run. A manual run (`POST /tasks/{id}/run`) drops a pending retry and starts at attempt 1; it follows the same retry,
alert and failure rules but does not move a future scheduled time.

Tasks that share one node: use `retry.attempts` of at least 2 (with a short backoff). Two tasks due at the same moment can
both see the node's runner as idle before one of them starts, and the other's attempt then fails with `node busy`; that
is a failed attempt by design, so without a retry it would count as a failed sequence.

Example with `"attempts": 3` and `"backoff": ["2m", "10m"]`: attempt 1 fails, wait 2 minutes, attempt 2 fails, wait 10
minutes, attempt 3 fails: one failure message, `streak` + 1, and the next scheduled run at the normal interval.

An attempt fails with a short reason such as `script not found: <name>`, `secret not configured: <names>`,
`no eligible node`, `ambiguous target`, `node busy`, `timeout` (the runner enforces the timeout; node-control cancels a run still reported as
running 10 s past it), the script's own error text, or `bad result: ...`.

When `alert` is set, the condition is evaluated on every successful run. A result that lacks the field or holds a
non-number there is a failed attempt (`bad result`), not a silent pass. A true condition sends the alert unless a cooldown
is running or the same message is being sent right now; only a successful send stores the cooldown, so after a failed
send the next run retries; a false condition clears the cooldown. ntfy sends
are retried up to three times (1 s, then 2 s) for transport errors, `429` and `5xx`. With `healthcheck` set the ping URL is
fetched after every successful sequence, and `<url>/fail` after a failed one.

Crash behaviour: the cooldown is stored only after ntfy accepted the message; while a send is under way, a second send of
the same message is held back in memory only. If node-control dies (or a deploy cuts a send off) between the send and
storing the cooldown, the next run sends the message again: a crash can duplicate an alert or a failure message, never
suppress one.

#### Lost runs, cancel and restart

- `lost` means node-control cannot tell how a run ended: the runner reported it unknown (it restarted and forgot it,
  `runner lost the run`) or could not be reached until 30 s past the timeout (`runner unreachable`). It is a failure
  like any other for retries and the streak.
- At start, a startup sweep finds runs a previous process left as `queued` or `running` (it was killed mid-attempt),
  cancels each on its node when the record says where it lives, and records it as `lost` with the reason
  `node-control restarted`. The sweep notifies nobody and changes no state, so the persisted attempt number is retried.
  Overdue tasks then start after a random delay of up to 10 s each, so they do not all start at once.
- A graceful shutdown cancels the running attempts on their nodes and records them as `failed` with the reason
  `shutdown`; they are not counted (not in metrics either) and nobody is told about a deploy. node-control gives all its
  modules one shared 5 s stop budget, so the tasks module waits at most 3 s for its attempts (each node cancel gets 1 s
  of that) and then closes Redis; a run whose cancel did not get through is found by the startup sweep of the next start.
  A notification still being sent when the process exits is abandoned; at worst it is sent again on a later run.
- Cancel (`DELETE /runs/{id}`) works on an attempt executing in this process. It cancels the run on its node, records
  `cancelled` and ends the sequence: no retry, no streak, no failure message, and the next run is scheduled normally.
  A cancel that races an attempt that has just finished may answer `204` while the run stays recorded as it ended.

### Metrics

With `TASKS_METRICS_ADDR` set, a read-only listener answers `GET /metrics` (Prometheus text format 0.0.4); every other
path is `404` and every other method `405`. It has **no authentication by design**, serves counters and gauges only, and
labels are limited to task ids, node names, run states and notification kinds (never a result value, an account, a secret
or a URL). Bind it on a private network only. The listener uses the machine's own network, not the tailnet, so with no
`[[services]]` in `fly.toml` it is reachable only from Fly's private network, which means any app in the same Fly
organisation network can read it. The content (ids, counts, states) is of low sensitivity but not secret. A listen error is logged once and the module
keeps working. Values are kept in memory: counters restart at zero with the process.

| metric | type | labels |
|---|---|---|
| `task_runs_total` | counter | `task`, `node`, `state` (finished attempts; an attempt cut short by a shutdown is not counted) |
| `task_run_duration_seconds` | summary (`_sum`, `_count`) | `task` |
| `task_alert_sent_total` | counter | `task`, `kind` (`alert`, `failure`, `ping`), `ok` (`true`, `false`) |
| `task_notify_failures_total` | counter | `task` |
| `task_failure_streak` | gauge | `task` |
| `task_last_success_timestamp_seconds` | gauge | `task` (absent until a success) |
| `task_next_run_timestamp_seconds` | gauge | `task` (the retry time while a retry is pending) |

The gauges of a task that leaves the config are dropped; its counters stay until restart. The heartbeat ping is counted
under `task="scheduler"`, `kind="ping"`. To have Fly scrape it, set the address and add a `[metrics]` block to `fly.toml`:

```toml
[env]
  TASKS_METRICS_ADDR = ":9091"

[metrics]
  port = 9091
  path = "/metrics"
```

Set `healthcheck` on every task. A node-control that crash-loops re-runs the interrupted attempt at each start (the
startup sweep records the old one as `lost` and the persisted attempt is retried), while the scheduler heartbeat still
pings at every start; only the per-task healthchecks (and the staleness rule below) notice that no task ever completes.

Useful alerts: `time() - task_last_success_timestamp_seconds > 2 * <every seconds>` (the same rule as `stale`),
`task_failure_streak >= 2`, and `increase(task_notify_failures_total[1d]) > 0` (a message that did not go out).

### Heartbeat

With `TASKS_HEARTBEAT_SECRET` set, node-control GETs the ping URL stored under that secret name once at start and then
every hour: a dead-man's switch in a healthchecks-style service alerts when node-control stops (or the whole scheduler is
stuck). The secret is read at each ping, so it can be pushed after start; the outcome is logged when it changes (a missing secret
at info level, a failed ping at warn level) and the next ping retries. The URL never appears in a log line, an answer or a metric label.

### Log events

Structured log lines (`log/slog`, key=value fields) with event names, ids and outcomes only, never a script, a param, a secret or a URL:
`run_started` (task, run, attempt), `node_chosen` (task, run, node, attempt), `attempt_result` (task, run, node,
attempt, state, dur_ms, reason), `retry_scheduled` (task, run, node, attempt, state, reason), `alert_evaluated` (task, hit,
outcome), `notify_result` (task, kind, outcome) and `ping_result` (task, kind, outcome; the heartbeat logs under task
`scheduler` and only when its outcome changes). Warnings logged once per process start: `ambiguous task target` (task,
node) and `runner rejected the token` (node). Store events are `tasks store open`, `tasks store unavailable, ...`,
`task store field unusable, ...` (field) and `task store write failed, serving from memory`.

### Trust model

The runner executes JavaScript supplied by whoever may push a config: treat `TASKS_ADMIN_USERS` as people who can run code
on your nodes. The code runs as a rootless podman container (read-only root file system, all capabilities dropped,
memory and pid limits, a timeout, one run at a time) started by an unprivileged service user, and the
`task-runner` Ansible role adds a kernel firewall rule that keeps that user (and so every container) off loopback, the LAN
and the tailnet while leaving the public internet open. Allow the runner port from node-control only in the tailnet ACL,
with the runner tag as the destination; node-control never sends the runner token or a task's secrets to a node without
that tag.
The public internet is open to the container, so a pushed script can send anything it can read, including the secrets it
was given, to any host. That is why only operators may push scripts and why `TASKS_ADMIN_USERS` matters.
Secrets are sent with each run over the tailnet and passed to the container through its environment. The runner never
writes them to its own files and removes the run's script directory afterwards; but while a container exists podman holds
its configuration, environment variables included, in the service user's storage, and the values are in the runner's
memory (and swap, if enabled). Treat the node's service user and disk as holding secrets transiently. node-control stores them encrypted in Redis, never returns them from
the API, never logs them, and scrubs them out of stored reasons and results.

### Operations

[`scripts/tasks.sh`](../../../scripts/tasks.sh) is the CLI client (it needs `curl` and `jq`, `sops` for secrets, and a
tailnet node whose user is in `TASKS_ADMIN_USERS`; `NODE_CONTROL_URL` is the base URL). Keep the config, the scripts and
the encrypted secrets in your private repo:

```
tasks.sh check  --file tasks.json --scripts scripts     # validate on the server (dry run)
tasks.sh diff   --file tasks.json --scripts scripts     # what a push would add (+), change (~), remove (-)
tasks.sh push   --file tasks.json --scripts scripts --secrets secrets.sops.json
tasks.sh pull   --file tasks.json --scripts scripts     # write the live state back to the files (no secrets)
```

`push` checks, shows the diff, asks, sends the secrets first (a task may only name secrets that are already stored),
then the config and scripts, and verifies the live state equals the files. Git is the long-term store of config, scripts and
encrypted secrets; node-control never reads git, it only holds the live copy in Redis. The header of the script lists the
flags, environment and exit codes.

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

These scripts only read (`profile-canary.sh` is described under "Compromised bridge").

- `scripts/parity.sh <python-url> <go-url>` fetches `/nodes.json`, `/status`, `/tailnet/status` and `/enroll/devices` from
  both apps, normalises JSON with `jq -S` and prints a unified diff; exit 1 on any difference. Needs `curl` and `jq`.
- `scripts/measure-memory.sh <fly-app> [seconds=300] [status-url]` samples the process RSS and `MemAvailable` every 10 s
  over `fly ssh console` while requesting `status-url`, then prints min `MemAvailable`, max RSS and PASS/FAIL against:
  `MemAvailable` above 20% of `MemTotal` and RSS below 205 MB. Run it on the trial app before cutover.

`measure-memory.sh` finds the process by its executable (`/usr/local/bin/node-control`, via `scripts/probe.sh`), requires
exactly one match and prints the pid and `comm` in every sample. `sh scripts/test_measure_memory.sh` tests that logic
and the argument checks without `fly`.

# node-control (Go)

node-control is a small web UI and JSON API. Use it to start, stop and inspect your exit nodes. It also gives optional user enrolment for a REALITY bridge proxy.

node-control is a single static Go binary that embeds a tailscale node (`tsnet`). The binary joins your headscale tailnet and listens **only on its tailnet address**:

- `:80` is the admin listener.
- `:8081` is the public enrolment listener, when `ENROLL_DB` is set.

node-control exposes nothing through the Fly proxy (`fly.toml` has no `[[services]]`).

A bridge is an entry host (xray/Reality front plus bridge agent) for clients on restrictive networks. For compatibility, these items keep the historical name `ingress`:

- The code.
- The `ingresses` JSON key.
- The `tag:ingress` headscale tag.
- The `AGENT_*` variables.
- The `/enroll/ingresses` endpoint paths.
- The Ansible role and service names.

node-control replaces the earlier Python implementation. It has the same HTTP API and the same enrolment database format.

Providers: Yandex Compute, OCI Compute, Selectel Cloud, Fly Machines.

## Environment

Put secrets in Fly secrets. Put plain settings in `fly.toml`.

| variable | purpose |
|---|---|
| `HEADSCALE_LOGIN_SERVER` | The headscale URL. It is the control server of the embedded node and the base of the node API. |
| `HEADSCALE_API_KEY` | Secret. It lists nodes for discovery and for the tailnet status. Without it, `/tailnet/status` answers `not_configured` and only explicitly configured nodes exist. |
| `TAILSCALE_AUTH_KEY` | Secret. The pre-auth key for the tailnet identity of this app. The tags come from the key, not from the client. |
| `TAILNET_HOSTNAME` | The name on the tailnet. The default is `node-control`. |
| `TSNET_VERBOSE` | `1` forwards the logs of the embedded tailscale node (a large quantity of log lines). By default, the app logs only messages for users, such as the login URL. At start, the app waits up to 60 s for the node to join the tailnet. If the node cannot join (bad auth key, control server not reachable), the app exits with a non-zero code. |
| `REDIS_URL` | Secret (Fly secret only, never in `fly.toml`). The plain `redis://` URL of the shared Redis on the Fly private network. When it is set, the tailscale state is in Redis, not in a file (see "Tailscale state"). When it is not set, the state is in a file under `/var/lib/tailscale`, as before. |
| `REDIS_KEY` | Optional hash key for the state. The default is `<FLY_APP_NAME or TAILNET_HOSTNAME>:tsnet` (for example `node-control:tsnet`). It must have the form `<app>:<name>` and must not contain `* ? [`. |
| `NODE_CONTROL_STATE_KEY` | Secret. 32 random bytes, base64 (`openssl rand -base64 32`). It encrypts the Redis state (AES-256-GCM). It is required with `REDIS_URL` unless `STATE_ALLOW_PLAINTEXT=1`. Without `REDIS_URL`, the app ignores it and logs a warning. |
| `STATE_ALLOW_PLAINTEXT` | `1` keeps the Redis state unencrypted. This is an explicit opt-out. We do not recommend it, because the state holds node private keys. |
| `STATE_IMPORT_FROM` | Optional path of a tailscaled or tsnet state file (for example `/var/lib/tailscale/tailscaled.state`). The app imports the file one time at start when the Redis hash is empty. In other conditions, the app ignores it. |
| `NODES_CONFIG` | Optional overrides file. The default is `/srv/app/nodes.json`. If the file does not exist, the app continues. If the file is malformed, the app does not start. |
| `ENROLL_ALLOWED_HOSTS` | Optional for the core. **Required** by the policy module (`POLICY_DB`) and the tasks module (on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set). Comma-separated Host values that the admin listener accepts. Any other value gets 400. Set it to block DNS rebinding. `*` allows any host (those two modules refuse it). `*.example.com` allows any host that ends in `.example.com` (list `example.com` too for the bare name), as in the Python app. |
| `ENROLL_DB` | The path of the SQLite database. It enables enrolment. Put it on the state volume: `/var/lib/tailscale/enroll.db`. |
| `ENROLL_REDIS` | `1` keeps the enrolment data in Redis, not in SQLite, so no volume is necessary (see "Enrolment in Redis"). It needs `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `ENROLL_SECRET_KEY`. The app then does not use `ENROLL_DB`. |
| `ENROLL_REDIS_KEY` | The key prefix of the enrolment data, `<app>:<name>`. The default is `<app>:enroll`. |
| `ENROLL_IMPORT_FROM` | With `ENROLL_REDIS=1`: the path of an old enrolment SQLite file. The app copies it into Redis one time, when Redis holds no enrolment data. The app opens the file read-only and does not change it. |
| `ENROLL_CONFIG` | The description of the enrolment bridges, for example `/srv/app/enroll.json`. It is required with `ENROLL_DB`. "Enrolment" gives the shape. |
| `ENROLL_SECRET_KEY` | Secret, at least 16 bytes. It derives subscription secrets. It is required with `ENROLL_DB`. |
| `AGENT_URL`, `AGENT_TOKEN` | The address (dialled through the tailnet) and bearer token of the agent of the first bridge. They are required with `ENROLL_DB` only for the old single-host `enroll.json`. In the list shape, they are the optional fallback of the FIRST entry (no `agent_url`, no `AGENT_TOKEN_<NAME>`). |
| `AGENT_TOKEN_<NAME>` | One bearer token for each bridge. `<NAME>` is the bridge name in upper case, with `-` as `_` (`ingress-2` reads `AGENT_TOKEN_INGRESS_2`). The `token_env` of an entry names another variable in its place. |
| `POLICY_DB` | The path of the SQLite change history. It enables the policy console (`/policy`). Put it on the state volume: `/var/lib/tailscale/policy.db`. It needs `HEADSCALE_LOGIN_SERVER` and `HEADSCALE_API_KEY`. |
| `POLICY_ADMIN_USERS` | Comma-separated tailnet logins that can use the console (required with `POLICY_DB`). Tagged nodes never have access. |
| `POLICY_REVERT_SECONDS` | The time in seconds to confirm a change before the automatic revert. From 30 to 900. The default is 120. |
| `POLICY_MAX_BYTES` | The largest policy that the console accepts. From 1024 to 4194304. The default is 262144. |
| `TASKS_*` | The scheduled-task module. It is on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set. "Tasks module" gives the variables and defaults. |
| `YANDEX_SA_KEY_JSON`, `YANDEX_FOLDER_ID` | The Yandex service account key, and the folder for the lookup by name. The account needs `compute.operator` and `compute.viewer`. |
| `OCI_USER_OCID`, `OCI_TENANCY_OCID`, `OCI_FINGERPRINT`, `OCI_PRIVATE_KEY`, `OCI_REGION`, `OCI_COMPARTMENT_ID` | The OCI credentials, and the default region and compartment. |
| `SELECTEL_ACCOUNT_ID`, `SELECTEL_PROJECT_ID`, `SELECTEL_USER`, `SELECTEL_PASSWORD`, `SELECTEL_REGION`, `SELECTEL_AUTH_URL` | The Selectel (OpenStack) service user, the default region and the optional auth URL. |
| `FLY_API_TOKEN` | The app-scoped deploy token for the controlled Fly app or apps. |

## Tailscale state

When `REDIS_URL` is set, the embedded node keeps its identity in one Redis hash, not in a file. Each field of the hash is a tailscale state key. The default hash is `node-control:tsnet`. `NODE_CONTROL_STATE_KEY` encrypts the values at rest (AES-256-GCM). The ciphertext is bound to the hash key and field, so a copy in a different location does not decrypt. A start with `REDIS_URL` and no key is an error, unless you set `STATE_ALLOW_PLAINTEXT=1`.

Redis rules (shared instance):

- Use plain `redis://` on the private network and database 0 only.
- The code sends only these commands: `HELLO 2`, `PING`, `HGET`, `HSET`, `HDEL`, `HLEN`, `HEXISTS` and, for `-force` only, `HKEYS` on the small state hash.
- go-redis always opens with `HELLO 2`. `HELLO 2 AUTH <user> <pass>` does the authentication and keeps the connection on RESP2.
- The code never sends `CLIENT`, `SELECT`, `FLUSH*`, `KEYS` or `SCAN`. A test records every command.
- The key has the app name as its prefix (`<app>:`) and has no TTL.
- Nothing polls Redis. The node writes state rarely.
- The connection uses the normal OS dialer, not the tailnet.
- If Redis is not reachable at start, the app stops with a non-zero exit code.
- Error text never contains the URL, the password or the key.

To move a current identity (so the node keeps its tailnet address), do one of these procedures:

- Set `STATE_IMPORT_FROM` to the current state file for the first start. The app imports the file only while the Redis hash is empty, and then ignores the setting.
- Run the import manually from `fly ssh console`:

```
node-control state-import -from /var/lib/tailscale/tailscaled.state [-force]
```

The importer reads the JSON of the file (state key -> base64) and writes every entry through the encryption layer. It reads each entry back to verify it, and it prints only a count. It refuses when Redis already holds state, unless you give `-force`. It never changes the source file. After a successful import, you can delete the old state file.

Interrupted import: before the importer writes, it keeps a marker field (`_node-control-import`) in the hash and removes it last. The marker is unencrypted and holds only the key names, never values. If an import fails, the importer rolls back what it wrote. If the process stops before the end (restart, kill), the marker stays.

The next start (or `state-import`) finds the marker and deletes the listed keys and the marker. It then does one of these:

- It does the import again, when `STATE_IMPORT_FROM` names a file that exists.
- It exits with a non-zero code and a message that says that it cleaned up the interrupted import.

It never starts the node on partial state. To recover, set `STATE_IMPORT_FROM` to the old state file and restart. Alternatively, run `node-control state-import -from <file>`. State that the node itself wrote has no marker, and the app always treats it as complete.

Move from plaintext to encrypted Redis state: the encrypted store refuses plaintext values.

> **WARNING:** Use `-force` only if you accept the loss of the previous identity. `-force` replaces the whole identity. The marker lists the keys of the file plus every key already in the hash. The importer deletes the old extra keys before it writes the new keys. A failed or interrupted forced import leaves no real keys at all: the previous identity is gone, and the error says so.

Do one of these procedures:

- Export the state to a file in the JSON format above (field name -> base64 value). Then run `state-import -from <file> -force` with the key set.
- Delete the hash and let the node register again with `TAILSCALE_AUTH_KEY`. The node then gets a new identity.

The enrolment data can also be in Redis (`ENROLL_REDIS=1`, below). Without it, the SQLite database (`ENROLL_DB`) stays on the volume, because it is a database file and a Machine mounts one volume. The policy console history (`POLICY_DB`) is still a SQLite file on the volume.

### Enrolment in Redis

`ENROLL_REDIS=1` replaces the SQLite file, so the machine needs no volume for enrolment. Environment:

- `REDIS_URL` and `NODE_CONTROL_STATE_KEY`. These are the same variables as for the tailscale state. That key encrypts the records. The store refuses to start without the key, unless `STATE_ALLOW_PLAINTEXT=1`.
- `ENROLL_SECRET_KEY` (unchanged). The app derives subscription secrets from it, the device id and the epoch.
- `ENROLL_REDIS_KEY` (optional).

Layout under the prefix `P` (all in DB 0):

- `P:meta` is a hash. `seq` is the last device id. The store never uses an id again.
- `P:d:<id>` is one hash for each device, with no TTL. It holds the sealed name, owner, client and UUIDs. It also holds plain `expires_at`, `revoked`, `epoch` and the activity numbers below. Thus revoke, extend and rotate are single atomic commands.
- `P:t:<hash>` is one hash for each one-time link, **with a TTL**.
- `P:approvals` is sealed.
- `P:acks` holds acknowledged attention items: a login, an item id and a hash. It is not sealed, like the audit log.

The key of a link expires through its TTL, and every spend also checks the stored expiry. A spend is one Lua script (link key and device key). The script writes nothing unless all of these conditions are true:

- Both keys exist.
- The device is not revoked or expired.
- The link has a use left and did not expire. (A bare `HINCRBY` on an expired key would create it again without a TTL.)

Commands that the store sends: `EVAL` (never `EVALSHA`), `HGET`, `HGETALL`, `HINCRBY`, `HSET`, `HDEL`, `HEXISTS`. The scripts also use `EXISTS`, `HGET`, `HDEL`, `HSET`, `HINCRBY` and `PEXPIRE`. Nothing scans, selects a database or flushes. A Redis error is an error, never an empty set: the sync to the agents does not push when the store cannot read its data.

Move a SQLite database that exists: do one of these procedures.

- Set `ENROLL_IMPORT_FROM` to the file. A start with no enrolment data in Redis copies it one time, verifies the copy and logs counts only.
- Run `node-control enroll-import -from <enroll.db> [-force]`.

The import opens the file read-only and never changes it, so the previous release can still run against it. The import keeps ids, epochs, UUIDs and link hashes without change, so issued subscription secrets and one-time links continue to work. `-force` overwrites records with the same ids and never deletes others. An interrupted import leaves no device counter, so the store still reads as empty and you can run the import again. A copy that fails the read-back verification removes the counter again (when the store was empty before). The next start then imports again and does not serve a copy that was not verified.

> **WARNING:** Encrypt the output of both exports before you keep it anywhere. Treat it as a secret, because it holds device names, owners and credentials.

Backup and restore:

- `node-control enroll-export [-to <file>]` writes the devices, UUIDs and approvals as JSON (stdout by default). It does not include one-time links, because Redis cannot list them without a scan.
- `node-control enroll-import -json <file|->` restores such a file into an empty Redis and verifies the copy. `-` reads stdin, so you can send an encrypted backup through a pipe with no plaintext file. Give exactly one of `-from` and `-json`. The command checks the shape of the JSON first. It refuses unknown fields, duplicate ids, unknown modes and empty UUIDs, and error texts never echo the file.
- `node-control enroll-export -format sqlite -to <new file>` writes a new SQLite enrolment database. The SQLite store (and so a release without Redis support) opens it as it is. The id counter stays ahead of every id that the store ever issued. The command refuses a file that exists, and it creates the new file with mode 0600. Use it to roll back.
`scripts/enroll-backup.sh` wraps both for a Fly deployment (it needs `flyctl` logged in and `jq`):

```sh
scripts/enroll-backup.sh backup  <fly-app> backup.sops.json --sops           # or: --age <recipient>
scripts/enroll-backup.sh restore <fly-app> backup.sops.json --sops [--force] # or: --age-identity <file>
```

`backup` does these steps:

1. It runs `enroll-export` in the machine and keeps the output in memory. It never writes an unencrypted file.
2. It checks the JSON shape, and it checks that the JSON holds at least one device.
3. It encrypts the output with `sops` or `age`.
4. It writes the result atomically, never over a file that exists, with mode 0600.
5. It checks that no device UUID is left in the encrypted result.
6. It prints counts only.

`--plaintext` is for a throwaway file, and you must give it explicitly. With `--sops`, the file name decides which `.sops.yaml` creation rule applies. The key names of the JSON stay readable, because sops encrypts values only. Export `SOPS_AGE_KEY` (or your sops key source) before you run the script.

`restore` decrypts the data and sends it into `node-control enroll-import -json -` in the machine (stdin through `fly ssh console -C`). That command checks the shape, and writes and verifies the copy. Without `--force`, a store that already holds data refuses.

Run a backup after enrolment changes. It is a manual step until an automatic one exists. `sh scripts/test_enroll_backup.sh` tests the script against a fake `fly` (it needs real `age` and `jq`, and no network).

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

`GET /` (Overview) · `GET /nodes` (page) · `GET /activity` (page) · `GET /nodes.json` · `GET /status` · `GET /nodes/{id}/status` · `POST /nodes/{id}/start` ·
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
  variable, `^[A-Z][A-Z0-9_]{0,63}$`; default `AGENT_TOKEN_<NAME>`, and for the first entry also `AGENT_TOKEN`),
  `node` (tailnet name of the machine the bridge runs on, a lowercase DNS name; needed when it differs from `name`,
  because the stopped check, the online flag and the Nodes page otherwise match the bridge by name), `intermittent`
  (`true` or `false`, default `false`; see "Intermittent bridges").
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

### Intermittent bridges

`"intermittent": true` is for a bridge that is expected to come and go. While its agent is `unreachable`, Attention
shows an info item instead of a critical one, and its row on the Nodes page has no At risk state and does not count
towards the lamp. Every other sync failure (`unauthorized`, `rejected`, `conflict`, `error`) stays critical, since a
reachable agent that refuses credentials is a real problem.

**The risk:** unreachable over the tailnet does not prove the bridge stopped serving clients. Its tailnet connection
can be down while its front still accepts connections, and then revocations and extensions made meanwhile are not in
effect on it: a revoked device may keep working there until the agent answers again. Only mark a bridge intermittent
if you accept that for it. A bridge stopped at its provider needs no such setting: it serves nobody, and it is
already an info item.

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

- `GET /enroll/ingresses` returns one row per bridge (`name`, `host`, `source`, `approval`, `active`, `approved`,
  `fingerprint`, the key, short id and machine key prefixes, `node_id`, `address`, `hostname`, `given_name`, `online`,
  `sync`, `detail`, `aliases`, `node`, `intermittent`) and, for a discovered bridge that has an approval, what that approval pinned:
  `approved_fingerprint`, `approved_node_id` and `approved_machine_key_prefix`. The review page (`/enroll/approve`)
  compares these with the values the bridge publishes now, so a `changed` bridge shows exactly what differs before it
  is approved again. The store keeps only these three, so nothing else can be compared.
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

## Console shell

Every admin page is wrapped in one shell: a sidebar of the pages available in this deployment, a top bar with a
search button, the top-bar actions and a freshness note, and a strip for changes that run against a deadline. The
shell is added to the HTML responses of the admin listener, inside the Host guard. It is never added to the public
listener, to JSON, to non-200 responses or to any request other than GET and HEAD. Its files (`hl.css`, `hl.js`,
`shell.js`, `palette.js`, `theme.js`, the fonts) are served under `/_ui/` and nothing is loaded from another origin.
With fewer than two visible entries the shell is compact: a header with the theme control and no sidebar. On a phone
the sidebar becomes a menu; the theme control (System, Light, Dark) sits inside it.

`/` is Overview: what needs the operator now and the health of each system. The node list moved to `/nodes`; `/`
shows Overview only when at least two other pages exist (with fewer, `/` goes straight to the one page). Activity is
`/activity`. The enrolment page is listed as Devices (`/enroll`).

`GET /_nav.json` returns the entries the caller may see as `[{"label","path","order","group","icon"},...]` with
`Cache-Control: no-store`.

### Sidebar entries: `Navigator`, `NavItem`, `Allow`

A module with an admin page adds entries by implementing `core.Navigator`. The core collects them after every module
registered, so a module that is switched off (return `nil`) or failed to register never appears:

```go
func (m *Module) Nav() []core.NavItem {
	return []core.NavItem{{Label: "Reports", Path: "/reports", Order: 30, Group: "operate", Icon: "tasks",
		Source: "reports", Allow: admins.Has}}
}
```

| `NavItem` field | meaning |
|---|---|
| `Label` | 1 to 24 printable characters (no control, bidi or zero-width characters) |
| `Path` | a local absolute path (`/x`, not `//x`, `http://x`, `javascript:x`; no `?`, `#`, colon, space, quotes or angle brackets) |
| `Order` | ascending; ties by label |
| `Group` | `""` (top, ungrouped), `operate` or `configure` |
| `Icon` | a name from `internal/ui/icons.go` (16 px line icon), or empty |
| `Allow` | `func(login string) bool`; nil means anyone who can reach the listener. Entries are filtered per caller, both in the sidebar and in `/_nav.json`. A caller that cannot be identified (or a tagged node) sees only entries without `Allow` |
| `Source` | the attention source (usually the module name) whose item count is shown beside the entry; empty means no count |

The longest matching path is the current page. Invalid entries and duplicate paths stop the start-up with an error
naming the module. `Allow` only decides what is shown: the module's own routes still check the caller.

### Top-bar actions: `ActionProvider`

A module implements `core.ActionProvider` (`Actions() []core.NavAction`, each `{Label, Path, Icon}`) to add a button to
the top bar, such as "Enrol device" (`/enroll?enrol=1`). An inactive module returns none. The actions also appear in
the command palette.

### Attention: `AttentionSource`

`core.AttentionSource` is `Attention(ctx, login) ([]AttentionItem, []SystemLine, error)`. Items are things that need
the operator; a system line is the one-line health of the module (`ok`, `waiting`, `critical`, `unknown`, `off`). Levels
are `timed` (has a `Deadline`), `critical`, `waiting` and `info`, most urgent first. Hue is used for attention only:
amber for waiting, red for at risk. `GET /_attention.json` merges all sources, sorts by level then age and returns
`{"items","systems","hidden_systems","updated_at"}`. All text is scrubbed and limited by the core, action paths must be
local, and no secret belongs in an item. A source that returns `core.ErrNotVisible` for this caller is left out and
`hidden_systems` is set. A source that fails or does not answer in time becomes a critical "could not be checked"
item, never its error text. Overview, the sidebar counts and the timed strip all read this one endpoint.

### Acknowledging an attention item

Someone who has looked at an item can acknowledge it: "I know, stop asking". It leaves the queue, the sidebar counts and
the lamp and no longer blocks "All clear", and sits in a collapsed "Acknowledged (N)" section on Overview with an Undo.
It stays in `/_attention.json` with `acknowledged: {by, at, until}`, and the system line follows the items that are left,
adding "N acknowledged" to its summary.

**Acknowledging fixes nothing and changes no behaviour.** A bridge that is out of sync stays out of sync, and revocations
stay not in effect there; the page just stops pointing at it, for everyone who can see the item (acknowledgements are
shared, not per person). The Nodes page still shows the state of the row.

An acknowledgement ends by itself, and the item is back in the queue, when:

- the item changes meaning: another level, or another reason (`AttentionItem.AckKey`, which a source sets to what the
  item means; the default is the id and the level). A bridge going from `unreachable` to `rejected` comes back; a count
  or an age changing in a title does not;
- its optional expiry passes (the page offers until it changes, 1 day and 7 days);
- the item clears: its source answered, without it. A source that errored or is hidden from the caller never ends one,
  nor does a module with a system line that reads `unknown`, as it could not fully look. This is only noticed while a
  console is open: an item that clears and returns while nobody is looking keeps its acknowledgement, if it means the same.

Who may do what: only someone who sees the item in their own attention list right now may acknowledge it or undo that.
`timed` items cannot be acknowledged, since they count down to something, and neither can the "could not be checked"
item of a source that does not answer: nothing can be known about it. The routes are `POST /_attention/acknowledge`
(`{"id", "ack_token", "expires_in_seconds"}`, up to 90 days, absent means until it changes) and
`POST /_attention/unacknowledge` (`{"id", "ack_token"}`), for a named person only, never a tagged node, and only with
`Content-Type: application/json`. `ack_token` is what the item said it was when the caller saw it, and a request naming
an older one is refused (409), so nobody acknowledges a change they have not seen. An item offers a token only when it
can be acknowledged.

Activity records acknowledging and undoing, with the actor, the time and the item's title; an acknowledgement ending by
itself is not recorded. Those entries are shown only to someone who can see the module that reported the item, which is
asked just as the attention list asks it, so a module that hides itself from a caller hides them too.

**Off without an enrolment store.** Acknowledgements live in the enrolment backend, next to the audit log: the
`attention_acks` table in SQLite or the hash `<prefix>:acks` in Redis. With neither (`ENROLL_DB` and `ENROLL_REDIS` unset)
there is nowhere to persist them, so no item offers it and no button shows. While the store cannot be read, every item
is shown as not acknowledged and nothing can be acknowledged. They are not part of `enroll-export`, `enroll-import` or the
backup script: after a restore every item is back in the queue.

### Activity: `ActivitySource`, `ActivityGate`

`core.ActivitySource` is `Activity(ctx, login, limit) ([]ActivityEntry, error)`; an entry is `{at, actor, source,
action, detail}`. `GET /_activity.json?limit=N&source=NAME` merges the sources newest first (default and maximum limits
are set in the core; `partial` is true when a source failed). The Activity page at `/activity` and the recent-activity
panel on Overview show it, with sentences from the one shared `activity-text.js`.

A source can also implement `core.ActivityGate` (`HasActivity() bool`) to say, without being asked for entries,
whether it keeps a log at all (it is active and has somewhere to read from). The Activity page and its sidebar entry
exist only when some source has a log; a source without the method counts as having one. Without the enrolment
backend, for example, node actions are not kept anywhere that can be read back, so the nodes module reports none.

Recorded actions come from: the enrolment audit log (device and bridge actions, node start and stop), the policy
`outcomes` table, the funnel audit log and the task run history.

### Search: `SearchSource`

`core.SearchSource` is `Search(ctx, login) ([]SearchItem, error)`; an item is `{label, kind, path}` (a device name, a
node id, a task id, a namespace). `GET /_search.json` merges them. A failing or hidden source contributes nothing, and
items with an empty label or a non-local path are dropped. The command palette (`Ctrl/Cmd+K`, or the search button)
filters the pages from `/_nav.json`, the top-bar actions and these items in the browser. No secret is ever a search
item.

### The enrolment audit log

With enrolment on, device and node actions (`device.create`, `device.link`, `device.revoke`, `device.delete`, `device.extend`, `device.rotate_secret`, `bridge.approve`, `bridge.unapprove`, and node start and stop) are recorded
as `core.AuditEntry` (`at`, `actor`, `action`, `detail`) in the enrolment backend: the `audit` table in SQLite, or the
list `<prefix>:audit` in Redis. It is capped at **1000** entries, newest first; older ones are dropped as new ones are
written. Acknowledging attention items (`attention.acknowledge`, `attention.unacknowledge`) is recorded here too. The detail holds names and dates only, never a link, key or subscription URL, and each field is cut to a fixed
length. A failure to write an entry is logged and never fails the action. The log is not part of `enroll-export`,
`enroll-import` or the backup script: **restoring a backup does not bring the audit log back**, and a restored
database starts with an empty one.

### Policy outcomes

The policy console records how each applied change ended in the `outcomes` table of its SQLite database: `confirmed`,
`reverted` (by the watcher at the deadline) or `reverted_on_request` (by an operator before it), with the actor
(`system` for the automatic revert) and the time. It holds no policy text. Activity and Overview read it. A change that
ended before the table existed has no record and is shown without an actor or time.

### Demo

`make demo SCENARIO=attention|clear|partial|minimal` (from this directory) runs `cmd/console-demo`: the whole console
on `127.0.0.1:8099` with invented data and local fakes for headscale, the enrolment agent, the task runner, a provider
and Redis. It listens on loopback only, refuses other addresses and is not part of the image. `attention` seeds one of
every kind of problem and one acknowledged item, with `bridge-4` on machine `vm-4`, marked intermittent and unreachable; `clear` seeds none, `partial` is `clear` seen by a caller who is in none of the `*_ADMIN_USERS`
lists (so the policy, tasks and funnel entries and systems are hidden), and `minimal` switches on the nodes module
only, so the console is the compact one.

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
The console adds a "Policy" entry to the sidebar. Its Content-Security-Policy (`ui.PageCSP`, like every admin page) allows
scripts and styles only from the page's own files: no inline script and no style attributes.

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
| `SESSIONS_TAG` | unset | headscale tag of the nodes that offer interactive browser sessions (same form as `TASKS_RUNNER_TAG`, for example `tag:browser-session`). Unset: no page, no routes, no nav item. Needs the tasks module to be on |
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


### Browser sessions (optional)

With `SESSIONS_TAG` set, the admin listener serves a **Browser** page (`/sessions`) where an admin starts and stops a
short-lived interactive browser on a tagged node and opens it from the tailnet. The node's `task-runner` does the work
(see its README for `RUNNER_SESSION_BIND`); node-control only relays, over the same bearer token and port as task runs.

| Route | What |
|---|---|
| `GET /sessions` | the page: one row per tagged node with a status dot, a length choice (30 min, 1 h, 4 h), Start, Stop, a countdown and an Open link while running. Refreshes every 5 s |
| `GET /sessions.json` | `{"nodes":[{"id","name","available","session":null or {"id","state","started_at","expires_at","port","reason"},"url"}]}`. A node whose runner cannot be reached shows `available: false`, never an error and never "no session" |
| `POST /sessions/{node}/start` | JSON body `{"ttl_seconds":N,"idle_seconds":N}` (both optional; defaults 3600 and 900; the runner enforces 60 to 14400 and 60 to 3600). `201` with the node view, `409` busy, `422` settings refused, `503` unreachable or sessions disabled on that node, `404` for a node without the tag |
| `POST /sessions/{node}/stop` | `204`, also when nothing is running; `404` for a node without the tag |

`{node}` is the node's headscale given name. A node without `SESSIONS_TAG` is never contacted and never receives the
runner token. The Open link points at the node's given name (or, when that is not a plain host name, its tailnet IPv4)
and the session port, over plain http inside the tailnet; no public address is ever used. Every start and stop is
logged with the node and the admin's login (`session started`, `session stopped`); the runner token and the session token never are. The link carries the session's access token (`path=websockify%3Ftoken%3D...`); it is
returned only to authorised admins, only for a running session, and scrubbed from error text.

Trust model: there is no stored browser profile. Reachability is gated by the ACL: allow the session port only to the
people who should have it. The session token (random per session, carried in the link, dies with the session) stops
other pages and users on allowed devices from using the endpoint. Starting and stopping is limited to the logins in `TASKS_ADMIN_USERS`. A session ends by
itself when its length runs out or it sits idle, and nothing is kept afterwards.

The token appears in the operator's browser history and in the page's address bar while the session is open, so do
not share the link; stopping the session invalidates it.

Behaviour to know:

- A running session makes the node report `busy`, so scheduled tasks pinned to that node fail with "node busy" or
  "no eligible node" until the session ends (up to its length).
- The Open link uses the node's tailnet name when it resolves on the operator's device (MagicDNS), else the tailnet
  IPv4. The published port follows the runner role's `task_runner_listen_addr`; an IPv6-only listen address is not
  supported by the link.
- The link may show before noVNC is listening; a click in the first seconds can be refused, retry.
- Closing the last browser window just reopens Chrome (a minimized window is listed in the desktop right-click menu,
  which has no Exit entry). A session ends by Stop, its length, the idle timeout, or if its display or bridge dies.
- A node whose runner has sessions disabled answers `404` to the status call and shows `available: false`.
- The link uses `resize=remote&quality=9`: the remote desktop is resized to the size of your browser tab (1:1 pixels,
  no scaling blur) and follows it when you resize the window. There is no maximum size (the container's memory limit
  is the bound). noVNC works in CSS pixels, so on a HiDPI display the browser still upscales the image by the device
  pixel ratio. Two tabs on one session with different sizes resize the shared desktop against each other.

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

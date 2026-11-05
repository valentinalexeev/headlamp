# node-control (Go)

node-control is a small web UI and JSON API. Use it to start, stop and inspect your exit nodes. It also gives optional user enrolment for a REALITY bridge proxy.

node-control is a single static Go binary that embeds a tailscale node (`tsnet`). The binary joins your headscale tailnet and listens **only on its tailnet address**:

- `:80` is the admin listener.
- `:8081` is the public enrolment listener, when `ENROLL_DB` or `ENROLL_REDIS=1` is set.

node-control exposes nothing through the Fly proxy (`fly.toml` has no `[[services]]`).

A bridge is an entry host (xray/Reality front plus bridge agent) for clients on restrictive networks. For compatibility, these items keep the historical name `ingress`:

- The code.
- The `ingresses` JSON key.
- The `tag:ingress` headscale tag.
- The `AGENT_*` variables.
- The `/enroll/ingresses` endpoint paths.
- The Ansible role and service names.

node-control replaces the earlier Python implementation. It has the same HTTP API and the same enrolment database format.

Providers: Yandex Compute, OCI Compute, OpenStack (Selectel and other OpenStack clouds), Fly Machines.

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
| `REDIS_KEY` | Optional hash key for the state. The default is `<FLY_APP_NAME or TAILNET_HOSTNAME>:tsnet` (for example `node-control:tsnet`). It must have the form `<app>:<name>` and must not contain `* ? [ \`. |
| `NODE_CONTROL_STATE_KEY` | Secret. 32 random bytes, base64 (`openssl rand -base64 32`). It encrypts the Redis state (AES-256-GCM). It is required with `REDIS_URL` unless `STATE_ALLOW_PLAINTEXT=1`. Without `REDIS_URL`, the app ignores it and logs a warning. |
| `STATE_ALLOW_PLAINTEXT` | `1` keeps the Redis state unencrypted. This is an explicit opt-out. We do not recommend it, because the state holds node private keys. |
| `STATE_IMPORT_FROM` | Optional path of a tailscaled or tsnet state file (for example `/var/lib/tailscale/tailscaled.state`). The app imports the file one time at start when the Redis hash is empty. In other conditions, the app ignores it. |
| `NODES_CONFIG` | Optional overrides file. The default is `/srv/app/nodes.json`. If the file does not exist, the app continues. If the file is malformed, the app does not start. |
| `ENROLL_ALLOWED_HOSTS` | Optional for the core. **Required** by the policy module (`POLICY_DB`) and the tasks module (on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set). Comma-separated Host values that the admin listener accepts. Any other value gets 400. Set it to block DNS rebinding. `*` allows any host (those two modules refuse it). `*.example.com` allows any host that ends in `.example.com` (list `example.com` too for the bare name), as in the Python app. |
| `ENROLL_DB` | The path of the SQLite database. It enables enrolment. Put it on the state volume: `/var/lib/tailscale/enroll.db`. |
| `ENROLL_REDIS` | `1` keeps the enrolment data in Redis, not in SQLite, so no volume is necessary (see "Enrolment in Redis"). It turns on enrolment, like `ENROLL_DB`. It needs `REDIS_URL`, `NODE_CONTROL_STATE_KEY`, `ENROLL_CONFIG` and `ENROLL_SECRET_KEY`. The app then does not use `ENROLL_DB`. |
| `ENROLL_REDIS_KEY` | The key prefix of the enrolment data, `<app>:<name>`. The default is `<app>:enroll`. |
| `ENROLL_IMPORT_FROM` | With `ENROLL_REDIS=1`: the path of an old enrolment SQLite file. The app copies it into Redis one time, when Redis holds no enrolment data. The app opens the file read-only and does not change it. |
| `ENROLL_CONFIG` | The description of the enrolment bridges, for example `/srv/app/enroll.json`. It is required with `ENROLL_DB` or `ENROLL_REDIS=1`. "Enrolment" gives the shape. |
| `ENROLL_SECRET_KEY` | Secret, at least 16 bytes. It derives subscription secrets. It is required with `ENROLL_DB` or `ENROLL_REDIS=1`. |
| `AGENT_URL`, `AGENT_TOKEN` | The address (dialled through the tailnet) and bearer token of the agent of the first bridge. They are required with `ENROLL_DB` or `ENROLL_REDIS=1` only for the old single-host `enroll.json`. In the list shape, they are the optional fallback of the FIRST entry (no `agent_url`, no `AGENT_TOKEN_<NAME>`). |
| `AGENT_TOKEN_<NAME>` | One bearer token for each bridge. `<NAME>` is the bridge name in upper case, with `-` as `_` (`ingress-2` reads `AGENT_TOKEN_INGRESS_2`). The `token_env` of an entry names another variable in its place. |
| `POLICY_DB` | The path of the SQLite change history. It enables the policy console (`/policy`). Put it on the state volume: `/var/lib/tailscale/policy.db`. It needs `HEADSCALE_LOGIN_SERVER` and `HEADSCALE_API_KEY`. |
| `POLICY_ADMIN_USERS` | Comma-separated tailnet logins that can use the console (required with `POLICY_DB`). Tagged nodes never have access. |
| `POLICY_REVERT_SECONDS` | The time in seconds to confirm a change before the automatic revert. From 30 to 900. The default is 120. |
| `POLICY_MAX_BYTES` | The largest policy that the console accepts. From 1024 to 4194304. The default is 262144. |
| `TASKS_*` | The scheduled-task module. It is on when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are set. "Tasks module" gives the variables and defaults. |
| `YANDEX_SA_KEY_JSON`, `YANDEX_FOLDER_ID` | The Yandex service account key, and the folder for the lookup by name. The account needs `compute.operator` and `compute.viewer`. |
| `OCI_USER_OCID`, `OCI_TENANCY_OCID`, `OCI_FINGERPRINT`, `OCI_PRIVATE_KEY`, `OCI_REGION`, `OCI_COMPARTMENT_ID` | The OCI credentials, and the default region and compartment. |
| `SELECTEL_ACCOUNT_ID`, `SELECTEL_PROJECT_ID`, `SELECTEL_USER`, `SELECTEL_PASSWORD`, `SELECTEL_REGION`, `SELECTEL_AUTH_URL` | The service user, the default region and the optional auth URL of the default OpenStack cloud (Selectel). |
| `OPENSTACK_CLOUDS` | Optional. Other OpenStack-compatible clouds, as `NAME=https://<keystone>/v3`, comma separated. `NAME` is upper case. The credentials of each cloud are `NAME_ACCOUNT_ID`, `NAME_PROJECT_ID`, `NAME_USER`, `NAME_PASSWORD` and `NAME_REGION`. Only this variable sets where these credentials go, so `nodes.json` cannot send them to a different URL. |
| `AWS_REGION`, `AWS_PROFILE`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_EC2`, `AWS_ENDPOINT_URL_AUTO_SCALING` | The default region for EC2 nodes, and the standard AWS credential chain. Use an instance or task role if possible, so that no key is necessary. The endpoint override is optional. See "AWS permissions". |
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
- `P:audit` is a list that holds the audit log (see "The enrolment audit log").

The key of a link expires through its TTL, and every spend also checks the stored expiry. A spend is one Lua script (link key and device key). The script writes nothing unless all of these conditions are true:

- Both keys exist.
- The device is not revoked or expired.
- The link has a use left and did not expire. (A bare `HINCRBY` on an expired key would create it again without a TTL.)

Commands that the store sends: `EVAL` (never `EVALSHA`), `HGET`, `HGETALL`, `HINCRBY`, `HSET`, `HDEL`, `HEXISTS`. For the audit log, it also sends `LPUSH` and `LTRIM` in one `MULTI`/`EXEC` transaction, and `LRANGE`. The scripts also use `EXISTS`, `HGET`, `HDEL`, `HSET`, `HINCRBY`, `PEXPIRE` and `DEL`.

Nothing scans, selects a database or flushes. A Redis error is an error, never an empty set: the sync to the agents does not push when the store cannot read its data.

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

You do not keep a list of nodes. node-control looks at each headscale node at every refresh. It caches the result for 15 seconds, and the node list and the tailnet status share one headscale fetch.

1. node-control lists a node if the node has the tag `tag:exit`. The UI and API id of the node is its reported `name`.
2. A node can also have `tag:cloud-<provider>` (`yandex`, `oci`, `openstack`, `aws`, `fly`). The old name `selectel` gives the same provider as `openstack` (deprecated, removed by 1.0.0). That provider then finds the VM whose name equals the node name:
   - OCI finds it by name in the compartment.
   - OpenStack finds it by name in the project and region.
   - AWS finds it by its `Name` tag in the region. Terminated instances do not count.
   - Yandex finds it by name in the folder.
   - Fly finds it by app name.

   If the provider finds the VM, the node is controllable. If not, `error` shows on that node only. Without a cloud tag, the node is status-only.
3. Provider settings are per provider and come from the environment (table above), not from the node.
4. Headscale keeps offline nodes, so a stopped VM stays in the list and you can start it. A node that you delete from headscale goes from the list.
5. Duplicate registrations (`name-1` next to `name`) fold into one entry. The online entry wins.

### AWS permissions

The `aws` provider controls a node in one of two ways:

- A plain EC2 instance: the provider starts and stops the instance.
- An instance in an Auto Scaling group, as `terraform/modules/exit-node-vm/aws` makes it. To stop the node, the
  provider sets the group to 0 instances. To start it, the provider sets the group to 1 instance. A stopped node of
  this type has no instance. The next start makes a new instance, which joins the tailnet again with the same name.
  See [ADR 0001](../../../docs/adr/0001-aws-exit-nodes-stop-by-scaling-the-group.md).

The provider finds the group from the instance, from the `asg` key in `nodes.json`, or from the name `<node name>-asg`.

Give node-control only the actions below. Limit the changes with a tag, so that node-control cannot change other
instances or groups:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["ec2:DescribeInstances", "autoscaling:DescribeAutoScalingGroups"], "Resource": "*"},
    {
      "Effect": "Allow",
      "Action": ["ec2:StartInstances", "ec2:StopInstances", "autoscaling:UpdateAutoScalingGroup"],
      "Resource": ["arn:aws:ec2:*:<your-account-id>:instance/*", "arn:aws:autoscaling:*:<your-account-id>:autoScalingGroup:*"],
      "Condition": {"StringEquals": {"aws:ResourceTag/headlamp-exit-node": "true"}}
    }
  ]
}
```

1. Put the tag `headlamp-exit-node=true` on each exit node instance or group. The `exit-node-vm/aws` module puts it on
   the group and on its instances.
2. Attach the policy to the role of the host that runs node-control.
3. If node-control runs outside AWS, attach the policy to a dedicated user, and set its keys as secrets.

The two describe actions do not support resource conditions, so they must have `"Resource": "*"`. They can read the
state of all instances and groups in the account, but they cannot change them.

### Optional overrides: `nodes.json`

The file is optional. Files from the Python version continue to work. Start from [`testdata/nodes.example.json`](testdata/nodes.example.json). The image has no `nodes.json` (the page then uses the default title). Lay your file over `app/` at deploy time.

- `title`, `subtitle`: the page heading.
- `nodes.<id>`: an override for a discovered node, or an explicit node. Explicit blocks win over discovery. Keys:
  - `label`: the UI text.
  - `hidden`: `true` removes the node from the page and API.
  - `tailnetName`: its headscale name. The default is the id.
  - `provider`, plus a block with the name of the provider, which holds connection details.

  Blocks:

  | provider | block |
  |---|---|
  | `yandex` | `{"instance_id"}` |
  | `oci` | `{"region", "compartment_id", "instance_name"}` or `{"instance_id"}` |
  | `openstack` (or the old name `selectel`) | `{"region", "instance_name"}` or `{"instance_id"}`. For a node in a different OpenStack cloud, add `"cloud": "NAME"`, with a `NAME` from `OPENSTACK_CLOUDS` |
  | `aws` | `{"region", "instance_name"}` (the `Name` tag), `{"region", "instance_id"}` or `{"region", "asg"}` |
  | `fly` | `{"app"}` |
- `aliases`: `{"old-id": "node-name"}`, so that bookmarks and scripts with an old id continue to work.
- `tailnetTrack`: extra headscale names to include in `/tailnet/status`.

## API

`GET /` (Overview) · `GET /nodes` (page) · `GET /activity` (page) · `GET /nodes.json` · `GET /status` · `GET /nodes/{id}/status` · `POST /nodes/{id}/start` · `POST /nodes/{id}/stop` · `GET /tailnet/status` · `GET /healthz`.

With `ENROLL_DB` or `ENROLL_REDIS=1`, these are also available: the `/enroll` admin pages and API on `:80`, and the public `/e/<token>` and `/sub/<secret>` on `:8081`.

Start and stop are idempotent. A provider failure shows as `error` on that node only.

## Enrolment (optional)

Set `ENROLL_DB` or `ENROLL_REDIS=1` to turn on user enrolment for a REALITY bridge proxy. The binary then serves a second, public enrolment listener next to the admin app. Both bind to the tailnet address only:

| port | serves | reachable by |
|---|---|---|
| 80 | admin app, with the `/enroll` admin pages | operators on the tailnet |
| 8081 | public enrolment | the bridge proxy only (restrict it in the tailnet ACL) |

Environment (secrets through Fly secrets, the rest in `fly.toml`):

- `ENROLL_DB`: the path of the SQLite database: `/var/lib/tailscale/enroll.db`. It must be on a Fly volume, or a deploy removes it. A Fly Machine mounts only one volume, so use the current `tailscale_state` volume (mounted at `/var/lib/tailscale`, see above). Do not create a second volume. The snapshots of that volume then also hold the enrolment database. The database holds device names and owners, but not the subscription secrets, which the app derives from `ENROLL_SECRET_KEY`. Treat the snapshots accordingly. Back up the database with `sqlite3 ... .backup` before risky changes.
- `ENROLL_REDIS=1`: keep the enrolment data in Redis (next subsection). The app then does not use `ENROLL_DB`.
- `ENROLL_CONFIG`: the path of a JSON file that describes the bridge nodes (shape below). Keep the real file in your private repo and overlay it.
- `ENROLL_SECRET_KEY`: at least 16 bytes. It derives the subscription secrets and the per-bridge credentials. A change to it invalidates both (see "Credentials per bridge").
- `AGENT_URL`, `AGENT_TOKEN`: the address and bearer token of the agent of the first bridge (the old single-host setup). Further bridges use `agent_url` in their entry (or node-control finds them through headscale) and `AGENT_TOKEN_<NAME>`.

- `ENROLL_ALLOWED_HOSTS` is optional for the core. It is required, and must not be a bare `*`, when the policy or tasks module is on. It holds comma-separated Host header values that the admin app accepts, for example `node-control.<tailnet-domain>,<tailnet-ip>`. Any other Host gets `400`. These are the same rules as `TrustedHostMiddleware` of Starlette, applied to the whole admin app on port 80. Set it: it prevents access to the admin API by a malicious web page, through DNS rebinding in the browser of an operator. List every name and IP that you use to open the page.

### `enroll.json`

The file gives one or more bridge nodes. Each entry is a bridge host with its own agent. A device gets a credential on every active bridge, and the phone profile lists them all.

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

- Top level:
  - `ingresses`: 1 to 8 entries, required.
  - `link_host`: required with more than one bridge. With one bridge, the default is the `host` of that bridge.
  - `profile_name`: the label of the imported profile. The default is `tunnel`.
  - Unknown keys are an error.
- Required keys of each entry:
  - `name`: `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, unique.
  - `host`: a unique DNS name.
  - `public_key`.
  - `short_id`.
  - `uuids`: see below.
- Optional keys of each entry:
  - `sni`: the default is `host`.
  - `port`: the default is 443.
  - `agent_url`: `http(s)://host:port`. Without it, the first entry uses `AGENT_URL`. For the others, the app uses headscale to find the `tag:ingress` node with the name of the entry, agent port 9000.
  - `token_env`: the name of the token variable, `^[A-Z][A-Z0-9_]{0,63}$`. The default is `AGENT_TOKEN_<NAME>`, and for the first entry also `AGENT_TOKEN`.
  - `node`: the tailnet name of the machine that runs the bridge, a lowercase DNS name. It is necessary when it is different from `name`. Otherwise, the stopped check, the online flag and the Nodes page match the bridge by name.
  - `intermittent`: `true` or `false`. The default is `false`. See "Intermittent bridges".
- `sni`, `short_id` and `public_key` must equal these values of the `reality-ingress` role: `reality_sni`, `reality_short_id` and the public half of `reality_private_key` (`xray x25519 -i <private>`). `port` must equal its `ingress_listen_port`.
- `link_host` is the host of subscription URLs and one-time links. Phones fetch `https://<link_host>/sub/<secret>` and `/e/<token>`. Thus the name must resolve to a bridge whose front serves it and proxies to `:8081` of node-control, with a certificate for it. Prerequisite: before you enrol the first device, make DNS for `link_host` available, and a certificate that works on the bridge that it points at. The front of every bridge must be able to serve it, so you can fail over if you move the name to another bridge.
- The old single-host file has `host`, `public_key`, `short_id`, `sni` and `port` at the top level, and no `ingresses`. It still loads as one bridge with the name `default` and `"uuids": "base"`, and uses `AGENT_TOKEN`. A file with both shapes is an error.

### Intermittent bridges

`"intermittent": true` is for a bridge that you expect to come and go. While its agent is `unreachable`, Attention shows an info item in place of a critical one. Its row on the Nodes page has no At risk state, and the lamp does not count it. Every other sync failure (`unauthorized`, `rejected`, `conflict`, `error`) stays critical, because a reachable agent that refuses credentials is a real problem.

> **WARNING:** Mark a bridge as intermittent only if you accept this risk for it. `unreachable` over the tailnet does not prove that the bridge no longer serves clients. Its tailnet connection can be down while its front still accepts connections. Revocations and extensions that you make during that time are then not in effect on it. A revoked device can continue to work there until the agent answers again.

A bridge that you stopped at its provider does not need this setting. It serves nobody, and it is already an info item.

### Credentials per bridge (`uuids`)

`uuids` is required on every bridge in the list shape. There is no default, because a value that depends on the other entries would change meaning when you add or remove one.

- `"base"`: the stored UUIDs of the device are the credential on this bridge. At most one bridge can use it: the bridge that exists today.
- `"derived"`: node-control computes the credential for each bridge and device mode with HMAC-SHA256, with `ENROLL_SECRET_KEY` as the key. Thus a credential from one bridge is useless on the others. A compromised bridge cannot compute the values of any other bridge.

Migration from the single-host file: write the list shape as follows.

1. Write the bridge that exists as an entry with `"uuids": "base"`. Its name does not matter for `base`: use `default` or any other name.
2. Write every new bridge as `"derived"`.

Phones continue to work without change, because their credential on the old bridge did not change.

Consequences to plan for:

- The bridge NAME is part of a derived credential. If you rename a `derived` bridge, you rotate its credentials, and phones need a new profile.
- If you change the order of entries, no credential changes. But the FIRST entry owns the `AGENT_URL` / `AGENT_TOKEN` fallback and the link view (key, short id and sni for import links). Thus, if you move another entry to the top, those variables reach a different agent.
- A rotation of `ENROLL_SECRET_KEY` invalidates the subscription secrets AND every derived credential. Publish the profiles again and enrol them again. `base` credentials survive.
- If you remove an entry from the file, the sync to it stops. That bridge keeps its last credential set, and a later revocation does not reach it. Take the host down or wipe its agent first.

### Sync with the bridge host

Every create, revoke or extend pushes the full credential set to the agent. A reconcile loop tries again every 5 minutes.

Activity (no connection log, no client addresses): each device row shows two values.

- **Enrolled** shows when a one-time link of the device was last spent, and how often.
- **Last active** shows when its traffic last increased.

A link spend marks the device in the same step as the spend (one SQLite transaction, one script on Redis). The app derives Last active from the traffic totals that the agents already report. It looks every 5 minutes (and at each page load) and records the time when the total of a device increased since the previous look.

The counters reset when xray restarts (the agent restarts it whenever the credential set changes). Thus a total that fell but is above zero also counts as activity since the reset. The first look at a device only gives the baseline. The app skips a look at which a bridge did not answer, because the absent counters would read as a reset. The resolution is therefore about 5 minutes. A device that was active only while node-control was down shows nothing for that time.

`GET /enroll/devices` rows carry `enrolled_at`, `enrol_count` and `last_active` (unix seconds, 0 = never). SQLite databases get the three columns on open. Redis device records without them read as 0. JSON and SQLite exports and imports carry them.

The buttons of a row depend on its state, so the page shows no useless button:

- A revoked device has only Delete.
- An expired device has Extend and Revoke.
- An active device that was never enrolled has New link, Extend and Revoke.
- An active device that is already enrolled has Extend and Revoke (it has its profile).

`POST /enroll/devices/{id}/links` answers `409` for a revoked or expired device, because a link for it would exist and never work.

Delete: `DELETE /enroll/devices/{id}` (JSON content type, like the other changes) permanently removes a **revoked** device with its UUIDs and links.

- node-control refuses an active or merely expired device with `409` ("revoke the device before deleting it").
- It refuses an unknown id with `404`.
- It never issues the id again.
- It pushes nothing to an agent (it only ever sends active devices).
- The `/enroll` page hides revoked devices by default ("Show revoked devices (N)", remembered in the browser). It offers only a Delete button on a revoked row.
- On Redis, the delete is one guarded script (`DEL` on the device key only when the key exists and the device is revoked). Links of the device expire on their own TTL and cannot be spent after the delete.

node-control logs a failed push (exception class, and the HTTP status for HTTP failures, never payloads or credentials). It classifies the failure for each bridge:

- `unreachable`: no connection.
- `unauthorized`: 401/403, `AGENT_TOKEN` mismatch.
- `rejected`: other non-2xx, typically 503, because xray refused the generated config.
- `conflict`: 409.
- `error`: an unexpected exception in the loop.

`GET /enroll/devices` returns `{"devices": [...], "sync": {"pending": bool, "state": "...", "since": <unix>|null}}`. Create, revoke and extend responses carry `sync_pending` and `sync`.

While `pending`, the `/enroll` page shows a banner: "bridge host out of sync since <time>: <state>". Until it clears, revocations and extensions are not in effect on the bridge host. node-control never pushes a store with no devices (state `empty`). The agent would refuse an unconfirmed empty set, and an empty store must never wipe the agent.

Every bridge has its own sync state and gets its own credential set (the derived credentials for a `derived` bridge). A failure on one bridge never fails the operation. node-control records a create, revoke or extend first and answers success. The response and the page show the bridge that is behind (`sync` aggregates, and `GET /enroll/ingresses` has one row for each bridge). The reconcile loop tries that bridge again.

> **WARNING:** Do not think that a revocation is complete while a bridge is behind. A revocation never blocks on an unreachable bridge. Until that bridge is back, the revoked credential still works there. The banner and the per-bridge rows tell you which bridge.

node-control merges traffic figures for each device across the bridges. When a bridge with a pending sync answered nothing, `stats_partial` is `true` and the page marks the column `Traffic*`: the numbers are a lower bound.

### Discovered bridges, approval and slow agents

node-control discovers nodes with the tag `tag:ingress` in headscale that `enroll.json` does not list, and shows them on the `/enroll` page. Discovery needs `link_host` in the file and `HEADSCALE_API_KEY`. The discovery line on the page says why discovery is off.

The app asks each discovered bridge for its public parameters: `GET /v1/info` on its agent, with the bearer token `AGENT_TOKEN_<NAME>`, where `<NAME>` is the node name. The app uses a discovered bridge only after the operator approves it. The approval pins the fingerprint of its published parameters AND the headscale node id that published them. Discovered bridges always use derived credentials. Approvals are in the enrolment database.

States of a row:

- `configured`: listed in `enroll.json`, approved by definition.
- `pending`: never approved.
- `approved`.
- `changed`.
- `unreachable`.
- `no token`.
- `invalid`.

To add a bridge, do these steps:

1. Deploy the host with the `reality-ingress` and `ingress-agent` roles.
2. Give its agent its own token, and set the `INGRESS_*` values so that `/v1/info` answers.
3. Make sure that its front serves `link_host` (see "Compromised bridge").
4. Register the node in headscale with `tag:ingress` and a name that fits `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`. This name becomes the bridge name.
5. Pin the name with `headscale nodes rename <id> <name>`. Use a name that the hostname of the machine does not produce. Otherwise, headscale follows a hostname change. `headscale nodes list` shows the id, addresses and names.
6. Set the token as a secret with the name `AGENT_TOKEN_<NAME>` and redeploy node-control. Alternatively, use an `enroll.json` entry (see above), which needs no approval.
7. Open `/enroll`. The bridge shows as `pending`, with these values:
   - Its host.
   - The node id.
   - The tailnet address and names.
   - A short prefix of its public key and short id.
   - A short prefix of its machine key.
   - A fingerprint.
8. Compare the node id, address, name and machine key with `headscale nodes list`.

   > **WARNING:** Make sure that the node itself is the node that you registered. An impostor that holds the name can publish the public values of the real bridge.

9. Use a channel that you trust to get the host, public key and short id from `GET /v1/info` on the agent. Compare them with the values on the page.

   The agent does not report a fingerprint. The fingerprint is the SHA-256 of node-control over the canonical JSON of `name`, `host`, `port`, `sni`, `public_key` and `short_id`.

10. Press Approve.

    The API is `POST /enroll/ingresses/<name>/approve` with `{"fingerprint": "<shown fingerprint>", "node_id": <shown node id>}`. The app refuses the request (409) if the fingerprint does not match the parameters that the bridge publishes at that moment. It also refuses (409) a node id other than the node that publishes them. `POST /enroll/ingresses/<name>/unapprove` removes the approval. Both POSTs need `Content-Type: application/json`.

11. Monitor the sync state on the row. The approval pushes the credential set immediately.

Phones get the new bridge when they refresh their subscription. For a profile imported by a one-time link, the person must refresh it.

- `GET /enroll/ingresses` returns one row for each bridge. The row has these fields: `name`, `host`, `source`, `approval`, `active`, `approved`, `fingerprint`, `node_id`, `address`, `hostname`, `given_name`, `online`, `sync`, `detail`, `aliases`, `node`, `intermittent`. It also has the key, short id and machine key prefixes. For a discovered bridge that has an approval, it also returns what that approval pinned: `approved_fingerprint`, `approved_node_id` and `approved_machine_key_prefix`. The review page (`/enroll/approve`) compares these with the values that the bridge publishes now. Thus a `changed` bridge shows exactly what is different before you approve it again. The store keeps only these three values, so the page cannot compare anything else.
- A `changed` bridge (its parameters are different, or another node id publishes them) is frozen. It is not in profiles and gets no sync. It keeps its old credential set until you approve it again or remove its approval. A revocation does not reach it during that time.
- Names in headscale are NOT proof of who owns a node. The given name starts as the hostname that the machine reports. Headscale derives it again when that hostname changes, while the given name still follows it. The given name follows it when it equals the hostname, or the hostname plus `-<digits>`. Only a name set with `headscale nodes rename <id> <name>` to a value that the hostname does not produce stays fixed. Thus:
  - **Set an explicit `agent_url` (the tailnet IP of the node, for example `http://100.64.0.12:9000`) on every configured bridge.** This removes the headscale lookup completely: the token goes only to that address, and no machine can change that address. (`AGENT_URL` does the same for the first entry.) The name lookup is only the fallback, and it is the weaker setting.
  - A CONFIGURED bridge without `agent_url` has this limit: the lookup accepts a tagged node only when its given name does not follow its hostname. Headscale gives the rule: the given name equals what headscale derives from the hostname, optionally with `-<number>` after it. The lookup also refuses hostnames that sanitise to nothing. Run `headscale nodes rename <id> <name>` on the bridge node first (the log says so when the app refuses a lookup). When several nodes share the name, the lookup fails and the app sends nothing to any of them.
  - The app identifies a DISCOVERED bridge by the node id AND the machine key of its approval, not by its name. The app finds the agent by that pair, and never sends its token or credentials to another node. The id alone is not sufficient, because ids can repeat after you rebuild or restore a headscale database. A node cannot choose its machine key. If the node list does not report machine keys, you cannot approve a bridge. Approvals made before the app pinned machine keys show `changed` ("approve again") until you approve them one more time. If that node goes away, the row becomes `invalid` ("the approved node is not present") and the app contacts nothing until you remove the approval. You can then review and approve a different node that now has the name.
> **WARNING:** Do not set the variable `AGENT_TOKEN_<NAME>` before you expect the node. Before approval, the app already sends this token (as the bearer of `GET /v1/info`) to a tagged node whose given name matches it. A pin of the node id after approval decreases this exposure but cannot remove it. Anyone who can register a tagged node with that name (and, for a host with no rename, a hostname that matches) receives that one token.

- A create, revoke, extend or approval answers after at most 3 seconds. If an agent did not answer by then, its row reads `pending` with state `unknown` until the push ends. The app already recorded the change itself, and that change never fails.

### Compromised bridge

Threat model, in short: a root attacker on one bridge host:

- Sees client source addresses and traffic for each credential.
- Sees its own Reality private key and the credentials that node-control pushes to it.
- Controls its own xray and agent (a revocation does not bind it).
- Sees every one-time link token and subscription secret that goes through it. The reason is that TLS ends on the front of the bridge, and plain HTTP then goes over the tailnet to node-control.

The attacker cannot reach the admin port of node-control or the agents of the other bridges, and cannot reach the tailnet through xray. Restrict both in the tailnet ACL: allow the agent port only from node-control, and `:8081` only from the bridge hosts.

What the design limits:

- Credentials are per bridge (`derived`), so credentials stolen from the agent or xray of a bridge are valid on that bridge only. This is true for a bridge that never served `link_host`.
- A bridge whose front serves `link_host` terminates TLS for profiles and subscription secrets. It sees complete profiles, with the credentials of every bridge (also the `base` credentials). Any host with the tag `tag:ingress` can fetch `:8081/sub/` with a secret that it collected.
- If such a bridge is compromised, do these steps:
  1. Rotate the subscription secrets and `ENROLL_SECRET_KEY` (this also changes every derived credential).
  2. Enrol again the devices that held `base` credentials.
- The `base` bridge holds the stored credentials. Keep it the bridge that you trust most, or migrate it to a derived entry in time.
- The app never uses a discovered bridge before approval, and the approval pins the fingerprint and the node id. A changed host stays frozen out of profiles and sync until you approve it again.
- The app asks each bridge with its own token and pushes to it only its own credential set. See the condition above for a bridge that serves `link_host`.

What it does not limit: a compromised bridge that serves `link_host` can give phones a modified profile (clients do not verify a signature). It can also read the tokens and secrets that go through it. The canary only detects this.

Detection: `scripts/profile-canary.sh <tailnet-base-url> <link-host> [<canary-sub-secret> | -]` fetches the profile of a dedicated canary device in two ways: directly (`<tailnet-base-url>:8081/sub/<secret>`) and through `https://<link-host>/sub/<secret>`. It compares the SHA-256 of the two bodies. It prints the two short hashes and `OK` or `MISMATCH`, never the body or the secret.

Exit codes: 0 identical, 1 different, 2 usage or fetch error. Each side must answer exactly 200 with a non-empty body. A redirect or an empty answer is an error.

To use the canary, do these steps:

1. Create a device only for the canary. The secret is the `<digits>.<token>` of the canary device.
2. Run the script from cron or CI on a machine on the tailnet.

> **WARNING:** Give the secret as the environment variable `CANARY_SECRET` or as `-` (one line on stdin). An argument shows in `ps`, and `sh -x` would print it.

A false `MISMATCH` is possible if the approved set of bridges changes between the two fetches. Run the script one more time before you act. It needs `curl` and `sha256sum` or `shasum`. `sh scripts/test_profile_canary.sh` tests it against local fakes.

The script only catches a bridge that serves the canary a different profile than all other clients. These bridges pass the check:

- A bridge that targets other phones.
- A bridge that only watches.
- A bridge that recognises the canary by its fixed `User-Agent` (`profile-canary`) or its source address and serves it the honest profile.

Structural fixes (not built), so that no bridge terminates TLS:

- Serve `link_host` from a host that is not on the data path of any bridge.
- Pass TLS through the front to a certificate on the side of node-control.

If a bridge is compromised, follow the incident runbook of the site (`ingress-compromise`). The runbook is in the private site repository that runs this deployment. It tells you to cut the host off, rebuild it, rotate what it held and enrol the affected devices again.

### What the bridge host sees

The one-time link (`/e/<token>`), its redirect and the subscription (`/sub/<secret>`) all go through the Caddy front of the bridge host. None of them carries the device name or owner: the deep link of the redirect uses the neutral `profile_name` as the profile label.

### Link previews

Messengers fetch a pasted link to build a preview. This would use a single-use link before the person taps it. `/e/<token>` and `/sub/<secret>` can answer a plain empty `404`, which does not consume the token. They do this when the User-Agent contains one of these values, without regard to case:

- `facebookexternalhit`
- `twitterbot`
- `telegrambot`
- `whatsapp`
- `slackbot`
- `discordbot`
- `skypeuripreview`
- `linkedinbot`
- `embedly`
- `bot`
- `crawler`
- `spider`
- `preview`
- `linkexpanding`
- `google-`
- `vkshare`
- `viber`

iMessage previews identify as `facebookexternalhit`/`Twitterbot`. Browsers (Safari, Chrome) and the client apps pass. An empty User-Agent, or no User-Agent, counts as a normal client. A preview bot with a User-Agent that is not in the list can still burn a link: create another link.

### Phone profile

The profile is a sing-box config:

- tun (`172.19.0.1/30`, `fdfe:dcba:9876::1/126`, auto/strict route).
- A selector `select` over the `auto`/`direct`/`exit` VLESS+Reality outbounds.
- DNS hijacked on the tun and resolved by `tls://1.1.1.1` through the selected outbound (`detour: select`, sing-box legacy DNS format, valid through 1.13).

The bridge hostname (the `server` of the VLESS outbounds) is the exception. A DNS rule `outbound: any` resolves it with the local resolver of the phone (`local` server, no detour). A lookup through `select` would need the outbound that needs the lookup. All other lookups go through the selected outbound.

Only the phone test verifies that this works and that only the bridge hostname reaches the local resolver. In `exit` mode, a site that the network of the client cannot reach must open. A DNS leak test must show only the resolver of the exit.

Differences from the Python app that this replaced:

- The app dials the agent through the embedded tailnet node.
- The app closes the database only after both listeners drain on shutdown.
- A misconfigured enrolment stops startup with a message that names the problem, never a secret. Examples are a variable that is not set, or an `ENROLL_CONFIG` that is not readable or not valid.

## Console shell

One shell wraps every admin page. The shell has these parts:

- A sidebar of the pages that are available in this deployment.
- A top bar with a search button, the top-bar actions and a freshness note.
- A strip for changes that run against a deadline.

The app adds the shell to the HTML responses of the admin listener, inside the Host guard. It never adds the shell to the public listener, to JSON, to non-200 responses or to any request other than GET and HEAD. The app serves the shell files (`hl.css`, `hl.js`, `shell.js`, `palette.js`, `theme.js`, the fonts) under `/_ui/` and loads nothing from another origin.

With fewer than two visible entries, the shell is compact: a header with the theme control and no sidebar. On a phone, the sidebar becomes a menu, and the theme control (System, Light, Dark) is inside it.

`/` is Overview: what needs the operator now and the health of each system. The node list moved to `/nodes`. `/` shows Overview only when at least two other pages exist. With fewer, `/` goes directly to the one page. Activity is `/activity`. The sidebar lists the enrolment page as Devices (`/enroll`).

`GET /_nav.json` returns the entries that the caller can see, as `[{"label","path","order","group","icon"},...]` with `Cache-Control: no-store`.

### Sidebar entries: `Navigator`, `NavItem`, `Allow`

A module with an admin page adds entries when it implements `core.Navigator`. The core collects them after every module registered. Thus a module that is switched off (return `nil`) or that failed to register never shows:

```go
func (m *Module) Nav() []core.NavItem {
	return []core.NavItem{{Label: "Reports", Path: "/reports", Order: 30, Group: "operate", Icon: "tasks",
		Source: "reports", Allow: admins.Has}}
}
```

| `NavItem` field | meaning |
|---|---|
| `Label` | 1 to 24 printable characters (no control, bidi or zero-width characters). |
| `Path` | A local absolute path (`/x`, not `//x`, `http://x`, `javascript:x`; no `?`, `#`, colon, space, quotes or angle brackets). |
| `Order` | From low to high. The label decides a tie. |
| `Group` | `""` (top, ungrouped), `operate` or `configure`. |
| `Icon` | A name from `internal/ui/icons.go` (16 px line icon), or empty. |
| `Allow` | `func(login string) bool`. nil means anyone who can reach the listener. The app filters entries for each caller, in the sidebar and in `/_nav.json`. A caller that the app cannot identify (or a tagged node) sees only entries without `Allow`. |
| `Source` | The attention source (usually the module name) whose item count shows next to the entry. Empty means no count. |

The longest path that matches is the current page. Entries that are not valid and duplicate paths stop the start with an error that names the module.

> **WARNING:** Do not use `Allow` as access control. `Allow` only decides what the page shows. The routes of the module must still check the caller.

### Top-bar actions: `ActionProvider`

A module implements `core.ActionProvider` (`Actions() []core.NavAction`, each `{Label, Path, Icon}`) to add a button to the top bar, such as "Enrol device" (`/enroll?enrol=1`). An inactive module returns none. The actions also show in the command palette.

### Attention: `AttentionSource`

`core.AttentionSource` is `Attention(ctx, login) ([]AttentionItem, []SystemLine, error)`. Items are things that need the operator. A system line is the one-line health of the module (`ok`, `waiting`, `critical`, `unknown`, `off`). Levels are `timed` (has a `Deadline`), `critical`, `waiting` and `info`, most urgent first. The console uses hue for attention only: amber for waiting, red for at risk.

`GET /_attention.json` merges all sources, sorts by level and then age, and returns `{"items","systems","hidden_systems","updated_at"}`. The core scrubs and limits all text, and action paths must be local. Do not put a secret in an item.

The core leaves out a source that returns `core.ErrNotVisible` for this caller, and sets `hidden_systems`. A source that fails or does not answer in time becomes a critical "could not be checked" item, never its error text. Overview, the sidebar counts and the timed strip all read this one endpoint.

### Acknowledge an attention item

A person who looked at an item can acknowledge it: "I know, stop asking". The item goes from the queue, the sidebar counts and the lamp, and no longer blocks "All clear". It stays in a collapsed "Acknowledged (N)" section on Overview with an Undo. It stays in `/_attention.json` with `acknowledged: {by, at, until}`. The system line follows the items that are left, and adds "N acknowledged" to its summary.

> **WARNING:** Do not acknowledge an item to correct a problem. **An acknowledgement fixes nothing and changes no behaviour.** A bridge that is out of sync stays out of sync, and revocations stay not in effect there. The page only no longer points at it, for everyone who can see the item (acknowledgements are shared, not per person). The Nodes page still shows the state of the row.

An acknowledgement ends by itself, and the item goes back in the queue, when one of these occurs:

- The item changes meaning: another level or another reason. `AttentionItem.AckKey` holds the meaning, which a source sets. The default is the id and the level. A bridge that goes from `unreachable` to `rejected` comes back. A change of a count or an age in a title does not bring it back.
- Its optional expiry passes (the page offers until it changes, 1 day and 7 days).
- The item clears: its source answered, without it. A source that sent an error or is hidden from the caller never ends an acknowledgement. A module with a system line that reads `unknown` also never ends one, because it could not fully look. The app notices a clear only while a console is open. An item that clears and returns while nobody looks keeps its acknowledgement, if it has the same meaning.

Permissions:

- Only a person who sees the item in their own attention list at that time can acknowledge it or undo that.
- You cannot acknowledge `timed` items, because they count down to something.
- You cannot acknowledge the "could not be checked" item of a source that does not answer, because nothing can be known about it.
- `POST /_attention/acknowledge` takes `{"id", "ack_token", "expires_in_seconds"}`. The expiry is up to 90 days. If it is absent, the acknowledgement stays until the item changes.
- `POST /_attention/unacknowledge` takes `{"id", "ack_token"}`.
- These routes are for a named person only, never a tagged node, and only with `Content-Type: application/json`.
- `ack_token` is what the item said it was when the caller saw it. The app refuses a request with an older token (409), so nobody acknowledges a change that they did not see.
- An item offers a token only when you can acknowledge it.

Activity records each acknowledgement and undo, with the actor, the time and the title of the item. Activity does not record an acknowledgement that ends by itself. The app shows those entries only to a person who can see the module that reported the item. It asks the module in the same way as the attention list does, so a module that hides itself from a caller hides them too.

**Off without an enrolment store.** Acknowledgements are in the enrolment backend, next to the audit log: the `attention_acks` table in SQLite or the hash `<prefix>:acks` in Redis. With neither (`ENROLL_DB` and `ENROLL_REDIS` not set), the app has no location to keep them, so no item offers an acknowledgement and no button shows.

While the app cannot read the store, it shows every item as not acknowledged, and you cannot acknowledge anything. Acknowledgements are not part of `enroll-export`, `enroll-import` or the backup script. After a restore, every item is back in the queue.

### Activity: `ActivitySource`, `ActivityGate`

`core.ActivitySource` is `Activity(ctx, login, limit) ([]ActivityEntry, error)`. An entry is `{at, actor, source, action, detail}`. `GET /_activity.json?limit=N&source=NAME` merges the sources, newest first. The core sets the default and maximum limits. `partial` is true when a source failed. The Activity page at `/activity` and the recent-activity panel on Overview show it, with sentences from the one shared `activity-text.js`.

A source can also implement `core.ActivityGate` (`HasActivity() bool`). This tells, without a request for entries, if the source keeps a log at all (it is active and has a location to read from). The Activity page and its sidebar entry exist only when a source has a log. A source without the method counts as a source with a log. For example, without the enrolment backend, the app keeps node actions nowhere that it can read back, so the nodes module reports none.

Recorded actions come from these sources:

- The enrolment audit log (device and bridge actions, node start and stop).
- The policy `outcomes` table.
- The funnel audit log.
- The task run history.

### Search: `SearchSource`

`core.SearchSource` is `Search(ctx, login) ([]SearchItem, error)`. An item is `{label, kind, path}` (a device name, a node id, a task id, a namespace). `GET /_search.json` merges them. A source that fails or is hidden gives nothing. The app drops items with an empty label or a non-local path.

The command palette (`Ctrl/Cmd+K`, or the search button) filters these in the browser: the pages from `/_nav.json`, the top-bar actions and these items. A secret is never a search item.

### The enrolment audit log

With enrolment on, the app records device and node actions as `core.AuditEntry` (`at`, `actor`, `action`, `detail`) in the enrolment backend. The actions are `device.create`, `device.link`, `device.revoke`, `device.delete`, `device.extend`, `device.rotate_secret`, `bridge.approve`, `bridge.unapprove`, and node start and stop. The backend location is the `audit` table in SQLite, or the list `<prefix>:audit` in Redis.

The log has a limit of **1000** entries, newest first. The app drops older entries when it writes new ones. It also records acknowledgements here (`attention.acknowledge`, `attention.unacknowledge`). The detail holds names and dates only, never a link, key or subscription URL. The app cuts each field to a fixed length. A failure to write an entry goes to the log and never fails the action.

> **CAUTION:** Do not expect a restore to give back the audit log. The log is not part of `enroll-export`, `enroll-import` or the backup script. **A restore of a backup does not bring the audit log back**, and a restored database starts with an empty log.

### Policy outcomes

The policy console records how each applied change ended, in the `outcomes` table of its SQLite database. The values are `confirmed`, `reverted` (by the watcher at the deadline) or `reverted_on_request` (by an operator before the deadline). The record has the actor (`system` for the automatic revert) and the time. It holds no policy text. Activity and Overview read it. A change that ended before the table existed has no record, and shows without an actor or time.

### Demo

`make demo SCENARIO=attention|clear|partial|minimal` (from this directory) runs `cmd/console-demo`. This is the whole console on `127.0.0.1:8099`, with invented data and local fakes for headscale, the enrolment agent, the task runner, a provider and Redis. It listens on loopback only, refuses other addresses and is not part of the image.

- `attention` seeds one of every kind of problem and one acknowledged item. `bridge-4` is on machine `vm-4`, marked intermittent and unreachable.
- `clear` seeds none.
- `partial` is `clear` as a caller sees it who is in none of the `*_ADMIN_USERS` lists. The policy, tasks and funnel entries and systems are hidden.
- `minimal` switches on the nodes module only, so the console is the compact one.

## Policy console (optional)

Set `POLICY_DB` to serve `/policy` on the admin listener for the logins in `POLICY_ADMIN_USERS`. Use the console to edit the headscale policy, preview the diff, apply and confirm. An applied change reverts by itself after `POLICY_REVERT_SECONDS` unless you confirm it. This also covers a restart of this app during that time.

The console refuses every route (403) for other users and for tagged nodes. It never logs policy text. Headscale must run its policy in database mode.

`ENROLL_ALLOWED_HOSTS` is required with `POLICY_DB` and must not contain a bare `*` (a `*.example.com` suffix is permitted). The Host guard of the core protects the console from DNS rebinding.

Confirm names the pending change. It succeeds only while headscale still serves the policy of that change.

"Revert now" in the pending banner undoes the pending change immediately, without a wait for its deadline. The "Revert" buttons in the history restore an older policy as a new change, so they are disabled while a change is pending.

> **WARNING:** If the page shows a warning that the automatic revert continues to fail, check headscale. Then use Confirm or Revert now as the way out. Confirm keeps the change. Revert now tries the revert again.

The console adds a "Policy" entry to the sidebar. Its Content-Security-Policy (`ui.PageCSP`, like every admin page) allows scripts and styles only from the files of the page: no inline script and no style attributes.

## Funnel module (optional)

Set `FUNNEL_DB` to make node-control the single admin surface of the `funnel-ingress` app (`apps/fly/funnel-ingress`). The ingress has no admin page of its own. It **pulls** its configuration from here and **reports** what it observes. Without `FUNNEL_DB`, the app registers nothing. The module needs the policy console (`POLICY_DB`), because every ACL change that it makes goes through the confirm-or-revert of the console. The same `ENROLL_ALLOWED_HOSTS` rule (no bare `*`) applies.

The Funnel page is `/funnel`, for users in `FUNNEL_ADMIN_USERS` only, never for tagged nodes. It has these functions:

- **Namespaces** (a name plus allowed port ranges).
  - When you create a namespace, change its ports or delete it, the module generates its ACL entries again. The entries are `tagOwners` for `<prefix><ns>` and one `ingress tag -> <prefix><ns>:<ports>` rule. The module submits them as one policy change.
  - The namespace is `pending` until you confirm that change, `active` after that, and `failed` if the change was reverted. The ingress only ever gets active namespaces.
  - A port change continues to serve the old ports until you confirm it.
  - The module refuses a delete while the namespace has manual routes, or the ingress still reports registered routes in it.
  - If the live policy has comments or trailing commas, the module refuses the change (edit the policy in the policy console). Nothing is dropped silently.
- **Publisher keys**. The module mints them through the headscale API for the user `FUNNEL_KEY_USER`, with the tags that the server fixes (`FUNNEL_TAG` plus `<prefix><ns>`). It ignores other request fields. It shows a key one time and never keeps, logs or audits it. The list shows keys with the namespace tag. Expiry works only on publisher keys.
- **Manual routes**. The namespace must be active. The target must be a headscale node (name or tailnet address) with the namespace tag. The port must be inside the ports of the namespace.
- **Blocked names**. The ingress removes a registered route for the name and refuses a new registration.
- The latest **ingress report** with its age (stale after 90 s), a DNS and certificate checklist for each namespace, and an audit log.

The ingress reaches two machine endpoints on the admin listener: `GET /ingress/v1/config` (with `ETag`, version = hash of the content) and `POST /ingress/v1/state` (1 MiB limit). It reaches them only as a node with `FUNNEL_INGRESS_TAG`. The module refuses a human there, and a tagged node on `/funnel/*`.

| variable | purpose |
|---|---|
| `FUNNEL_DB` | The path of the SQLite database. It enables the module. Put it on the state volume, for example `/var/lib/tailscale/funnel.db`. |
| `FUNNEL_PUBLIC_DOMAIN` | The lower-case DNS name that holds the namespaces (`<service>.<ns>.<domain>`). It must not be under the headscale base domain. |
| `FUNNEL_INGRESS_HOSTNAME` | Optional name that the wildcard record points at, for example `<app>.fly.dev` (it shows in the checklist). |
| `FUNNEL_TAG`, `FUNNEL_NS_TAG_PREFIX`, `FUNNEL_INGRESS_TAG` | Tags: publishers, namespace prefix, the ingress node. The defaults are `tag:funnel`, `tag:ns-`, `tag:funnel-ingress`. |
| `FUNNEL_TAG_OWNERS` | The owners that the module writes into the generated `tagOwners` entries (comma-separated users or groups). |
| `FUNNEL_KEY_USER` | The headscale user NAME that owns publisher keys (resolved to an id when the module mints a key). |
| `FUNNEL_ADMIN_USERS` | The tailnet logins that can use the page and API (required). |
| `FUNNEL_RESERVED_PORTS` | The ports that the ingress uses itself. The default is `8080,9100`. No namespace can allow them (the ingress would refuse the whole configuration). |

> **WARNING:** Do not use `FUNNEL_TAG_OWNERS` to control who can mint keys. Headscale does not check tag ownership when a key is created or used. The controls are the fixed tags and the user gate.

## Tasks module (optional)

The module gives scheduled checks that drive a browser script on one of your nodes, with retries, alerts and observability. A task names a script, a node (or a set of tags), a schedule and what to do with the result.

node-control selects an eligible node and gives the script and its secrets to the [`task-runner`](../../task-runner/README.md) on that node. It follows the run to its end and applies the retry, alert and notification rules. The script is generic: it opens pages, reads a value and returns a small JSON object. This repository holds nothing about what the task checks.

```
 operator ──tasks.sh / /tasks page──▶ node-control (tasks module) ──── HGET/HSET ────▶ Redis
 (tailnet user)                       │  config, scripts, secrets,                    hash node-control:tasks
                                      │  state and run history, scheduler,            (values encrypted)
                                      │  alerts, metrics, heartbeat
                                      │  start / poll / cancel, over the tailnet
                                      ▼
                                   node (tailnet) ── task-runner :9100 ── rootless container: Chromium + script
```

The module is in `internal/tasks`. The admin listener (`:80`) serves the admin API and page, gated by the tailnet identity of the caller. A Redis failure never stops node-control. The tasks routes answer `503` until the store opens. The module tries again in the background, after 30 s, then 1, 2 and every 5 minutes. After the store opens, the schedule continues from memory.

### Activation and environment

The module is on only when `REDIS_URL`, `NODE_CONTROL_STATE_KEY` and `TASKS_RUNNER_TOKEN` are all set. Otherwise, it logs `tasks module inactive: not set: ...` with the names of the absent variables, and stays off (no routes, no top-bar entry). When the module is on, a bad value stops startup with a message that names the variable and never a secret. `REDIS_URL` and `NODE_CONTROL_STATE_KEY` are the same as for the Tailscale state (see above). The tasks data is always encrypted, and `STATE_ALLOW_PLAINTEXT` does not apply to it.

| variable | default | purpose |
|---|---|---|
| `REDIS_URL` | | Secret. The shared Redis (see the table above). |
| `NODE_CONTROL_STATE_KEY` | | Secret. It encrypts every stored value (AES-256-GCM, each value bound to its Redis field name). |
| `TASKS_RUNNER_TOKEN` | | Secret. The bearer token that the node runners accept (their `RUNNER_TOKEN`). |
| `TASKS_ADMIN_USERS` | | Comma-separated tailnet logins that can use the page and API. Required when the module is on. Tagged nodes never have access. |
| `TASKS_RUNNER_TAG` | `tag:task-runner` | The headscale tag that a node must have to run any task (matches `^tag:[a-z0-9][a-z0-9-]{0,62}$`). The module never probes nodes without it, and they never receive the runner token or the secrets of a task. |
| `SESSIONS_TAG` | unset | The headscale tag of the nodes that offer interactive browser sessions (same form as `TASKS_RUNNER_TAG`, for example `tag:browser-session`). Unset: no page, no routes, no nav item. It needs the tasks module to be on. |
| `TASKS_RUNNER_PORT` | `9100` | The port of the runner on the nodes, 1 to 65535. |
| `TASKS_NTFY_URL` | `https://ntfy.sh` | The base URL of the ntfy server for alerts and failure messages. |
| `TASKS_POLL_SECONDS` | `5` | How often an attempt that runs asks its runner for the status, 1 to 60. |
| `TASKS_METRICS_ADDR` | unset | `host:port` of the metrics listener, for example `:9091` (host can be empty, port numeric). Unset: no listener. |
| `TASKS_HEARTBEAT_SECRET` | unset | The NAME of a stored secret (letters, digits, `_`, at most 64) whose value is a heartbeat ping URL. Unset: no heartbeat. |

The module requires the Host guard of the admin listener, like the policy console. When the module is on, `ENROLL_ALLOWED_HOSTS` must list the host names that the admin listener answers to. An unset, empty or `*` value stops startup. The guard protects the page and API from DNS rebinding.

Rollout:

1. Give every runner node the runner tag in headscale.
2. Write the tailnet ACL rule that opens the runner port, with that tag as the destination.

The module ignores a node without the tag, even when a task names it.

The tag is what node-control trusts with the runner token and the task secrets. It is trusted because the `tagOwners` of the policy decides who can have it when a node registers (`tailscale up --advertise-tags` is checked against it). On 0.28 and later, only an admin changes the tags of a node after that (`headscale nodes tag` or the API). The storage of tags depends on the headscale version, and node-control reads both forms:

- headscale 0.28 and later (unified model): a node has one `tags` list. Tags and user ownership exclude each other. A tag on a user-owned (tag-less) node removes its user ownership, and only a new authentication with empty tags undoes that.
- Older headscale (legacy model): forced tags, which an admin sets, plus valid tags. A node can request valid tags at any time with `--advertise-tags`, and gets them when `tagOwners` lets its user own them.

> **WARNING:** List ONLY the admin or service identity as owner of the runner tag in `tagOwners`, never human users whose devices are not runners. The `tagOwners` entry of the policy is the trust boundary for the runner tag.

The safest way to tag a node is as an admin. Use `headscale nodes tag`, `scripts/headscale-tag-node.sh` below, or a pre-auth key with that tag as its ACL tag. A node that only advertises the tag gets it only if the policy lets its user own it.

The helper needs `HEADSCALE_URL` and `HEADSCALE_API_KEY`, or `HEADSCALE_API_KEY_CMD` that prints the key. To use the helper, do these steps:

1. Run `scripts/headscale-tag-node.sh show --node <given-name>`.
2. Read the model that it prints (`unified` or `legacy`).
3. Run `scripts/headscale-tag-node.sh add --node <given-name> --tag tag:task-runner` without `--yes` to see the plan.

   The plan is `tags: [...] -> [...]`, or `forced: [...] -> [...]` on a legacy server.

4. Run the same command with `--yes` to apply it.

   The command keeps the other tags of the node. It must do this because the API replaces the whole set: `tags` on the unified model, the forced tags on the legacy model. It then reads the node again to check the result.

5. Run `show` again. The runner tag shows under `tags` (unified) or `forced` (legacy).

> **WARNING:** On the unified model, a tag on a node with no tags removes its user ownership. For this reason, the helper also requires `--allow-ownership-change` there.

A given name follows the reported hostname of the node until you set it with `headscale nodes rename` (see "Names in headscale" above). Thus rename the runner nodes if `node` targets must not depend on what a runner reports.

### Task configuration

The configuration is one JSON object, `{"tasks": {"<id>": {...}}}`. The decode is strict: unknown fields, trailing data, a duplicate task id and a bad value are all errors. The module reports every problem with its task and field.

A task id matches `^[a-z0-9][a-z0-9._-]{0,63}$`. `config` and `secrets` are reserved, because they would clash with the API paths. `scheduler` is also reserved, because it is the label of the heartbeat in metrics and logs. Durations are Go duration strings (`"90s"`, `"12h"`). The module refuses a bare number, because its unit would be a guess.

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

A complete placeholder file is in [`examples/private-repo/fly/node-control/tasks/tasks.example.json`](../../../examples/private-repo/fly/node-control/tasks/tasks.example.json).

| field | required | default and rules |
|---|---|---|
| `script` | yes | The file name of a stored script: no `/`, `\` or `..`. A push also needs the name to match `^[A-Za-z0-9._-]{1,64}$`. |
| `params` | no | A JSON object that the script gets as `params`. |
| `target` | yes | Either `node` or `tags`, never both. `node` is the headscale **given name** of the node, exactly. The module never matches the host name that a node reports, because any node can claim any host name. With `tags`, a node must have all of them. If two nodes have the same given name, the target is ambiguous: the attempt fails with `ambiguous target` and the module contacts nothing. In both cases, only nodes with the runner tag (`TASKS_RUNNER_TAG`) are candidates. `online: true` skips nodes that headscale reports offline. |
| `schedule.every` | yes | At least `1m`. |
| `schedule.jitter` | no | `0s`. The next run is `now + every ± jitter` (uniform), never sooner than `every / 2`. |
| `timeout` | no | `3m`. From `1s` to `30m` (the runner counts whole seconds). The runner enforces it. |
| `retry.attempts` | no | `1`. 1 to 10 attempts for each sequence. |
| `retry.backoff` | with attempts > 1 | A list of positive durations. Entry *n* is the wait after failed attempt *n*. The last entry repeats. |
| `secrets` | no | The names of stored secrets that the script gets as `secrets.<NAME>`. The runner gives secrets and params to the container in environment variables. Thus the module refuses a push when the JSON of the secret values of a task plus its params JSON is more than 100000 bytes. |
| `healthcheck` | no | The NAME of a secret that holds a healthchecks-style ping URL. |
| `alert.when` | with `alert` | `field` (a numeric result field), `op` (one of `<` `<=` `>` `>=` `==` `!=`) and `value` (number). |
| `alert.cooldown` | no | `24h`. It must be positive. |
| `alert.notify`, `on_failure.notify` | with the block | `topic` (a secret NAME: the ntfy topic is never in the config. `ntfy.sh` is a public service where the topic name is the only access control, so keep a long random topic as the secret), `priority` (`min`, `low`, `high`, `max`; any other value is the ntfy default), `title`, `message`. |
| `on_failure.cooldown` | no | `24h`. It must be positive. |

The module replaces `{name}` in a title or message. In an alert, it uses the scalar result field of that name. In a failure message, it uses `task` and `reason`. An unknown name stays as written, so a mistake in the name is visible. `{task}` is also available in alert text. The module refuses a placeholder with a colon.

A candidate node for a task has the runner tag and matches `target` (and is online when `online` is set). Its runner must answer its health check as reachable and idle. A candidate node must also have a tailnet IPv4 address. Among the candidates, the module prefers the node that last ran the task successfully, then the least busy node, then the lowest name. If none is eligible, the attempt fails with `no eligible node`. A runner that refuses the token (`401`) counts as unhealthy. node-control logs `runner rejected the token` with the name of the node, one time for each process start.

### Script contract

A script is a CommonJS module that exports `run` (`module.exports = { run }` or `module.exports = fn`). The call is `run({ page, params, secrets, log })`:

- `page` is a Playwright page of a new headless Chromium context (runs share nothing).
- `params` and `secrets` are objects.
- `log(msg)` writes a line to the log tail of the run. That tail is visible only from `GET /v1/runs/{id}` (`log_tail`) of the runner itself. node-control does not keep or show it.

The script returns a small JSON object. The runner refuses a result that is not an object or is larger than 4096 bytes (the run fails and says why). To fail the run, throw or return a rejected promise. The task-runner README gives details and the isolation of the container. A commented example is in [`examples/private-repo/fly/node-control/tasks/scripts/check-balance.example.js`](../../../examples/private-repo/fly/node-control/tasks/scripts/check-balance.example.js).

The module scrubs secret values (replaced by `***`) from the reason and the result of the run before it keeps or shows them.

> **WARNING:** Do not return a secret from a script, even though the module scrubs secret values.

### HTTP API

Every route is on the admin listener and needs a tailnet user from `TASKS_ADMIN_USERS`. Anyone else, and every tagged node, gets `403` with an empty body. While the store is not open, each route except the page and its static files answers `503 {"error":"degraded"}`. Errors have the shape `{"error":"..."}`. A push that fails validation adds `"problems": [...]`.

Every `POST` and `PUT` needs `Content-Type: application/json`, also `POST /tasks/{id}/run`, which has no body. Otherwise, the answer is `422 {"detail":"Content-Type must be application/json"}`. For example:

`curl -X POST -H 'Content-Type: application/json' -d '{}' "$NODE_CONTROL_URL/tasks/check-balance/run"`

Answers are `Cache-Control: no-store`. Times are RFC 3339 UTC.

| method and path | answer |
|---|---|
| `GET /tasks` | The HTML page (static files at `/tasks/static/tasks.css` and `tasks.js`), served also while degraded. |
| `GET /tasks.json` | A list of task views, sorted by id (`[]` before the first config). |
| `GET /tasks/{id}` | One task view; `404 {"error":"unknown task"}`. |
| `POST /tasks/{id}/run` | Start a run now: `202 {"run_id":"..."}`; `409` a run already executes; `404` unknown task; `503` the app is in shutdown. |
| `GET /tasks/{id}/runs` | The newest 50 runs of the task, newest first, as run views; `404` unknown task. |
| `GET /runs/{id}` | One run view, from the attempts that execute now and the history of every task; `404 {"error":"unknown run"}`. |
| `DELETE /runs/{id}` | Cancel a run that executes: `204`; `409` when the run is recorded as running or queued but does not execute in this process; `404` unknown or finished. |
| `GET /tasks/config` | `{"config": {...}, "scripts": {"name": "text"}}`: the live config (defaults applied) and the scripts that it references; `404 {"error":"no config"}` before the first push. |
| `PUT /tasks/config` | Body `{"config": {...}, "scripts": {"name": "text"}}`. Add `?dry_run=1` (or `true`) to validate only. Answer `200 {"tasks": {added, changed, removed}, "scripts": {added, changed, removed}, "dry_run": bool}`, plus `"pending_write": true` when the module could not write to Redis yet. `413` for a body over 1 MiB or a script over 256 KiB. `422` for an invalid body, a script name outside `^[A-Za-z0-9._-]{1,64}$`, an invalid config (`problems`), a reserved task id, a task that names a secret that is not stored (**push secrets first**), a task whose secrets and params exceed the 100000-byte limit of the runner, or a script that is not in the body and not stored. |
| `GET /tasks/secrets` | `[{"name": "...", "set": true}]`. Values are never returned. |
| `PUT /tasks/secrets` | Body `{"NAME": "value", ...}`: at most 64 names (`^[A-Za-z0-9_]{1,64}$`), each value 1 to 4096 bytes, body at most 64 KiB. `200 {"stored": [names]}` (plus `pending_write`); `413`; `422` with a message that never echoes a value or a bad name. |

Task view:

- `id`, `script`, `target`, `every`.
- `running` (bool).
- `running_run` (a run view or null).
- `last_success`.
- `last_result` (result of the newest successful run).
- `streak`.
- `attempt` (above 0 while a retry is pending).
- `next_run` (the retry time during a retry sequence).
- `stale`.

A task is stale when it is overdue for a success. This is true in two conditions:

- Its last success is older than twice `every`.
- It never succeeded, and it failed for real or its first run is more than a minute late. A cancel, or a run that a shutdown or restart cut short, does not count as a failure here.

Run view:

- `id`, `task`, `node`, `attempt`.
- `state` (`queued`, `running`, `succeeded`, `failed`, `lost`, `cancelled`).
- `reason`, `started`, `ended`.
- `duration_ms` (null until the run ended).
- `result`.
- `script_sha` (SHA-256 of the script that ran).
- `notify_outcome` (for example `alert=sent ping=sent`; `suppressed` when a cooldown held the message back).

The API never exposes the location of a run on its node.

A push replaces the config and the scripts that it references as a whole. Nothing deletes a stored script or secret: they stay, unreferenced, until you remove them from Redis manually. Consequences:

- `tasks.sh` always sends every referenced script. A raw API caller can PUT a config that names a script without its text in `scripts`. The module then silently uses the stored text of that name, even an old orphan.
- If you remove a task id, its state and history stay in Redis. `GET /tasks.json`, `GET /tasks/{id}`, `GET /tasks/{id}/runs` and `GET /runs/{id}` only look at configured tasks, so they no longer show them. If you add the same id again, that state continues: its streak, a pending retry, its next run and the alert and failure-message cooldowns. Use a new id for a new start.

### Storage

All data is in one Redis hash, `node-control:tasks`, on the shared Redis. `NODE_CONTROL_STATE_KEY` encrypts each value, and each value is bound to its field name, so a value copied to another field does not decrypt. Fields: `config`, `script:<name>`, `secret:<name>`, `secrets-index`, `state:<task>` and `runs:<task>`.

The store reads every field one time when it opens, and then serves all data from memory. After that, it only writes (one `HSET` for each changed field), and it reads with `HGET` only at open. Nothing polls Redis, which is important on a Redis billed per command.

Timestamps that drive behaviour (next run, retry time, last success, the alert and failure-message cooldowns) are absolute times. Thus a restart continues exactly where it stopped, and an expired cooldown needs no cleanup. When a write fails, the value stays in memory, and the next write tries it again (the API says `pending_write`). The scheduler continues with Redis down.

The module can find a stored value that it cannot use. The value may not decrypt, for example after a change of `NODE_CONTROL_STATE_KEY`. The config may no longer pass validation, or a state or history field may not decode. This does not keep the module closed. The module logs the field by name (`task store field unusable, ...`, never its value) and treats it as absent:

- A bad config reads as no config.
- A secret that the module cannot read is not listed.
- A bad state or history starts empty.

`tasks.sh push` (with its secrets) repairs it. Only a failure to reach Redis keeps the routes at `503`.

History has limits. The module keeps the newest 200 runs for each task. It also drops the oldest runs to keep the whole `runs:<task>` field under 256 KiB. The module cuts a stored reason at 512 bytes, and keeps a result larger than 8 KiB as `{"truncated": true}`.

### Retry, failure and alert semantics

A *sequence* is the attempts that one scheduled (or manual) run makes. An attempt that fails with attempts left stores the attempt number and its due time, and returns. Nothing sleeps: the scheduler wakes at that time, and a restart continues the backoff.

Only the last failed attempt of a sequence counts. It increments `streak`, and sends the failure message (when `on_failure` is set, at most once per its cooldown) and the failure ping. A success resets `streak` and schedules the next run. A manual run (`POST /tasks/{id}/run`) drops a pending retry and starts at attempt 1. It follows the same retry, alert and failure rules, but does not move a future scheduled time.

Tasks that share one node: use `retry.attempts` of at least 2, with a short backoff. Two tasks due at the same moment can both see the runner of the node as idle before one of them starts. The attempt of the other task then fails with `node busy`. That is a failed attempt by design, so without a retry it would count as a failed sequence.

Example with `"attempts": 3` and `"backoff": ["2m", "10m"]`:

1. Attempt 1 fails. The wait is 2 minutes.
2. Attempt 2 fails. The wait is 10 minutes.
3. Attempt 3 fails. The result is one failure message, `streak` + 1, and the next scheduled run at the normal interval.

An attempt fails with a short reason, such as:

- `script not found: <name>`.
- `secret not configured: <names>`.
- `no eligible node`.
- `ambiguous target`.
- `node busy`.
- `timeout`. The runner enforces the timeout. node-control cancels a run that is still reported as running 10 s after it.
- The error text of the script.
- `bad result: ...`.

When `alert` is set, the module evaluates the condition on every successful run. A result without the field, or with a non-number there, is a failed attempt (`bad result`), not a silent pass.

A true condition sends the alert, unless a cooldown runs or the same message is in the send process at that time. Only a successful send stores the cooldown, so after a failed send the next run tries again. A false condition clears the cooldown. The module makes at most three attempts for an ntfy send: the first send and two retries. It retries only after a transport error, `429` or `5xx`. The wait is 1 s before the first retry and 2 s before the second retry.

With `healthcheck` set, the module fetches the ping URL after every successful sequence, and `<url>/fail` after a failed one.

Crash behaviour: the module stores the cooldown only after ntfy accepted the message. During a send, the module holds back a second send of the same message in memory only. The app can stop (or a deploy can cut a send off) between the send and the store of the cooldown. The next run then sends the message again. A crash can duplicate an alert or a failure message, but can never suppress one.

#### Lost runs, cancel and restart

- `lost` means that node-control cannot tell how a run ended. The runner reported it unknown (it restarted and forgot it, `runner lost the run`), or could not be reached until 30 s after the timeout (`runner unreachable`). It is a failure like any other for retries and the streak.
- At start, a startup sweep finds runs that a previous process left as `queued` or `running` (the process was killed during an attempt). The sweep cancels each on its node when the record gives its location, and records it as `lost` with the reason `node-control restarted`. The sweep notifies nobody and changes no state, so the module retries the persisted attempt number. Overdue tasks then start after a random delay of up to 10 s each, so they do not all start at the same time.
- A graceful shutdown cancels the attempts that run on their nodes, and records them as `failed` with the reason `shutdown`. They are not counted (also not in metrics), and nobody is told about a deploy. The app gives all its modules one shared 5 s stop budget. Thus the tasks module waits at most 3 s for its attempts (each node cancel gets 1 s of that) and then closes Redis. The startup sweep of the next start finds a run whose cancel did not get through. The module abandons a notification that is in the send process when the process exits. At worst, a later run sends it again.
- Cancel (`DELETE /runs/{id}`) works on an attempt that executes in this process. It cancels the run on its node, records `cancelled` and ends the sequence. There is no retry, no streak and no failure message, and the next run is scheduled normally. A cancel that races an attempt that just finished can answer `204` while the run stays recorded as it ended.

### Metrics

With `TASKS_METRICS_ADDR` set, a read-only listener answers `GET /metrics` (Prometheus text format 0.0.4). Every other path is `404` and every other method is `405`. It has **no authentication by design** and serves counters and gauges only. Labels are limited to task ids, node names, run states and notification kinds (never a result value, an account, a secret or a URL).

> **WARNING:** Bind the metrics listener on a private network only, because it has no authentication. The listener uses the network of the machine, not the tailnet. With no `[[services]]` in `fly.toml`, only the Fly private network can reach it. Thus any app in the same Fly organisation network can read it. The content (ids, counts, states) is of low sensitivity but not secret.

The module logs a listen error one time and continues to work. Values are in memory: counters restart at zero with the process.

| metric | type | labels |
|---|---|---|
| `task_runs_total` | counter | `task`, `node`, `state` (finished attempts; an attempt that a shutdown cuts short is not counted) |
| `task_run_duration_seconds` | summary (`_sum`, `_count`) | `task` |
| `task_alert_sent_total` | counter | `task`, `kind` (`alert`, `failure`, `ping`), `ok` (`true`, `false`) |
| `task_notify_failures_total` | counter | `task` |
| `task_failure_streak` | gauge | `task` |
| `task_last_success_timestamp_seconds` | gauge | `task` (absent until a success) |
| `task_next_run_timestamp_seconds` | gauge | `task` (the retry time while a retry is pending) |

The module drops the gauges of a task that leaves the config. Its counters stay until restart. The module counts the heartbeat ping under `task="scheduler"`, `kind="ping"`. To let Fly scrape it, set the address and add a `[metrics]` block to `fly.toml`:

```toml
[env]
  TASKS_METRICS_ADDR = ":9091"

[metrics]
  port = 9091
  path = "/metrics"
```

Set `healthcheck` on every task. A node-control in a crash loop runs the interrupted attempt again at each start. The startup sweep records the old one as `lost`, and the module retries the persisted attempt. The scheduler heartbeat still pings at every start. Only the per-task healthchecks (and the staleness rule below) notice that no task ever completes.

Useful alerts:

- `time() - task_last_success_timestamp_seconds > 2 * <every seconds>` (the same rule as `stale`).
- `task_failure_streak >= 2`.
- `increase(task_notify_failures_total[1d]) > 0` (a message that did not go out).

### Heartbeat

With `TASKS_HEARTBEAT_SECRET` set, node-control sends a GET to the ping URL stored under that secret name, one time at start and then every hour. A dead-man's switch in a healthchecks-style service alerts when node-control stops (or the whole scheduler is stuck). The module reads the secret at each ping, so you can push it after start. It logs the outcome when the outcome changes: an absent secret at info level, a failed ping at warn level. The next ping tries again. The URL never shows in a log line, an answer or a metric label.

### Log events

The module writes structured log lines (`log/slog`, key=value fields). They hold event names, ids and outcomes only, never a script, a param, a secret or a URL:

- `run_started` (task, run, attempt).
- `node_chosen` (task, run, node, attempt).
- `attempt_result` (task, run, node, attempt, state, dur_ms, reason).
- `retry_scheduled` (task, run, node, attempt, state, reason).
- `alert_evaluated` (task, hit, outcome).
- `notify_result` (task, kind, outcome).
- `ping_result` (task, kind, outcome). The heartbeat logs under task `scheduler` and only when its outcome changes.

Warnings logged one time for each process start: `ambiguous task target` (task, node) and `runner rejected the token` (node). Store events are `tasks store open`, `tasks store unavailable, ...`, `task store field unusable, ...` (field) and `task store write failed, serving from memory`.

### Trust model

> **WARNING:** Treat `TASKS_ADMIN_USERS` as people who can run code on your nodes. The runner executes JavaScript from anyone who can push a config.

The code runs as a rootless podman container. The container has a read-only root file system, all capabilities dropped, memory and pid limits, a timeout, and one run at a time. An unprivileged service user starts it. The `task-runner` Ansible role adds a kernel firewall rule that keeps that user (and so every container) off loopback, the LAN and the tailnet. The rule leaves the public internet open.

In the tailnet ACL, allow the runner port from node-control only, with the runner tag as the destination. The app never sends the runner token or the secrets of a task to a node without that tag.

> **WARNING:** Let only operators push scripts. The public internet is open to the container, so a pushed script can send anything that it can read to any host. This includes the secrets that it got. For this reason, `TASKS_ADMIN_USERS` is important.

node-control sends secrets with each run over the tailnet, and the runner gives them to the container through its environment. The runner never writes them to its own files, and removes the script directory of the run after the run. But while a container exists, podman holds its configuration, also the environment variables, in the storage of the service user. The values are also in the memory of the runner (and swap, if enabled).

> **WARNING:** Treat the service user and disk of the node as temporary holders of secrets.

node-control keeps secrets encrypted in Redis, never returns them from the API, never logs them, and scrubs them out of stored reasons and results.

### Operations

[`scripts/tasks.sh`](../../../scripts/tasks.sh) is the CLI client. It needs `curl` and `jq`, `sops` for secrets, and a tailnet node whose user is in `TASKS_ADMIN_USERS`. `NODE_CONTROL_URL` is the base URL. Keep the config, the scripts and the encrypted secrets in your private repo:

```
tasks.sh check  --file tasks.json --scripts scripts     # validate on the server (dry run)
tasks.sh diff   --file tasks.json --scripts scripts     # what a push would add (+), change (~), remove (-)
tasks.sh push   --file tasks.json --scripts scripts --secrets secrets.sops.json
tasks.sh pull   --file tasks.json --scripts scripts     # write the live state back to the files (no secrets)
```

`push` does these steps:

1. It does the check and shows the diff.
2. It asks you.
3. It sends the secrets first, because a task can only name secrets that are already stored.
4. It sends the config and scripts.
5. It verifies that the live state equals the files.

Git is the long-term store of config, scripts and encrypted secrets. node-control never reads git, and only holds the live copy in Redis. The header of the script lists the flags, environment and exit codes.

### Browser sessions (optional)

With `SESSIONS_TAG` set, the admin listener serves a **Browser** page (`/sessions`). On this page, an admin starts and stops a short-lived interactive browser on a tagged node and opens it from the tailnet. The `task-runner` of the node does the work (see its README for `RUNNER_SESSION_BIND`). The app only relays, over the same bearer token and port as task runs.

| Route | What |
|---|---|
| `GET /sessions` | The page: one row for each tagged node with a status dot, a length choice (30 min, 1 h, 4 h), Start, Stop, a countdown and an Open link while the session runs. It refreshes every 5 s. |
| `GET /sessions.json` | `{"nodes":[{"id","name","available","session":null or {"id","state","started_at","expires_at","port","reason"},"url"}]}`. A node whose runner cannot be reached shows `available: false`, never an error and never "no session". |
| `POST /sessions/{node}/start` | JSON body `{"ttl_seconds":N,"idle_seconds":N}` (both optional; defaults 3600 and 900; the runner enforces 60 to 14400 and 60 to 3600). `201` with the node view. `409` busy, or more than one node has this name. `422` settings refused. `503` node list not available, node offline, node without a tailnet IPv4 address, node not reachable, or sessions disabled on that node. `404` for an unknown name or a node without the tag. |
| `POST /sessions/{node}/stop` | `204`, also when nothing runs. `404` for an unknown name or a node without the tag. `409` more than one node has this name. `503` node list not available, node offline, node without a tailnet IPv4 address, or node not reachable. |

`{node}` is the headscale given name of the node. The app never contacts a node without `SESSIONS_TAG`, and that node never receives the runner token.

The Open link points at the given name of the node and the session port. When the given name is not a plain host name, the link uses the tailnet IPv4. The link uses plain http inside the tailnet. The link never uses a public address. The app logs every start and stop with the node and the login of the admin (`session started`, `session stopped`). It never logs the runner token or the session token.

The link carries the access token of the session (`path=websockify%3Ftoken%3D...`). The app returns it only to authorised admins, only for a session that runs, and scrubs it from error text.

Trust model:

- There is no stored browser profile.
- The ACL controls who can reach the session. Allow the session port only to the people who must have it.
- The session token is random for each session, is in the link, and ends with the session. It prevents use of the endpoint by other pages and users on allowed devices.
- Only the logins in `TASKS_ADMIN_USERS` can start and stop a session.
- A session ends by itself when its length runs out or when it stays idle, and nothing is kept after that.

> **WARNING:** Do not share the link. The token shows in the browser history of the operator and in the address bar of the page while the session is open. A stop of the session invalidates the link.

Behaviour to know:

- A session that runs makes the node report `busy`. Scheduled tasks pinned to that node then fail with "node busy" or "no eligible node" until the session ends (up to its length).
- The Open link uses the tailnet name of the node when it resolves on the device of the operator (MagicDNS). Otherwise, it uses the tailnet IPv4. The published port follows `task_runner_listen_addr` of the runner role. The link does not support an IPv6-only listen address.
- The link can show before noVNC listens. A click in the first seconds can be refused: try again.
- If you close the last browser window, Chrome only opens again. The desktop right-click menu lists a minimized window, and has no Exit entry. A session ends by Stop, its length, the idle timeout, or if its display or bridge stops.
- A node whose runner has sessions disabled answers `404` to the status call and shows `available: false`.
- The link uses `resize=remote&quality=9`. The remote desktop changes to the size of your browser tab (1:1 pixels, no blur from a change of scale). It follows the tab when you resize the window. There is no maximum size (the memory limit of the container is the limit).

noVNC works in CSS pixels, so on a HiDPI display the browser still enlarges the image by the device pixel ratio. Two tabs on one session with different sizes resize the shared desktop against each other.

## Build and run

```
make build                 # bin/node-control, CGO off
docker build -t node-control .
```

The image is `alpine` (so a shell is available for `fly ssh console`), with the binary at `/usr/local/bin/node-control` and `app/` at `/srv/app`. `ARG TSVERSION` only documents the tailscale.com release, and fails the build if `go.mod` pins another one. Without `REDIS_URL`, the tailscale state is on the volume mounted at `/var/lib/tailscale` (see `fly.toml`). With `REDIS_URL`, the state is in Redis (see "Tailscale state"). Deploy with the overlay procedure from the repository `CLAUDE.md` (section 2.5).

## Tests

```
go vet ./... && go test ./...
go test -race ./...
```

The tests need no cloud access: providers and headscale are fake HTTP servers, and the core runs on plain TCP, not on tsnet.

## Trial, cutover and canary helpers

These scripts only read (see "Compromised bridge" for `profile-canary.sh`).

- `scripts/parity.sh <python-url> <go-url>` fetches `/nodes.json`, `/status`, `/tailnet/status` and `/enroll/devices` from both apps. It normalises JSON with `jq -S` and prints a unified diff. It exits with 1 on any difference. It needs `curl` and `jq`.
- `scripts/measure-memory.sh <fly-app> [seconds=300] [status-url]` samples the process RSS and `MemAvailable` every 10 s over `fly ssh console` while it requests `status-url`. It then prints min `MemAvailable`, max RSS and PASS/FAIL against these limits: `MemAvailable` above 20% of `MemTotal` and RSS below 205 MB. Run it on the trial app before cutover.

`measure-memory.sh` finds the process by its executable (`/usr/local/bin/node-control`, through `scripts/probe.sh`). It requires exactly one match and prints the pid and `comm` in every sample. `sh scripts/test_measure_memory.sh` tests that logic and the argument checks without `fly`.

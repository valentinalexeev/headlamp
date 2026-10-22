# node-control

A small web UI + JSON API to start, stop and inspect the nodes you list in a config file. It runs on
Fly.io, joins your tailnet and listens **only on its tailscale IP** (nothing is exposed through Fly's
proxy). Providers: Yandex Compute, OCI Compute, Selectel Cloud, Fly Machines.

## Config: `app/nodes.json`

Keep the real file in your private repo and lay it over the placeholder before `flyctl deploy` (the
overlay procedure in `CLAUDE.md`). Start from [`nodes.example.json`](nodes.example.json); the format
is documented in `app/nodes.py`. Each node has an id (used in the API paths), a `provider`, an optional
`tailnetName` (its headscale name, default = the id), optional UI overrides, and a block named after
the provider with the connection details:

| provider | block | notes |
|---|---|---|
| `yandex` | `{"instance_id"}` | |
| `oci` | `{"region", "compartment_id", "instance_name"}` or `{"instance_id"}` | by default the instance is found **by name** in the compartment on every call, so replacing the VM needs no change; `instance_id` pins one instance |
| `selectel` | `{"region", "instance_name"}` or `{"instance_id"}` | OpenStack; found by name like `oci` |
| `fly` | `{"app"}` | a single-machine app |

## Credentials (Fly secrets / env, never in the config)

- Yandex: `YANDEX_SA_KEY_JSON` (service account key JSON; the `compute.operator` role is enough)
- OCI: `OCI_USER_OCID`, `OCI_TENANCY_OCID`, `OCI_FINGERPRINT`, `OCI_PRIVATE_KEY` (and `OCI_REGION` if a node
  does not set `region`). Policy for name lookup: `use instances in compartment <name>` (includes list/get and
  start/stop) plus `read virtual-network-family` as needed; a pinned `instance_id` can be scoped to one instance.
- Selectel: `SELECTEL_ACCOUNT_ID`, `SELECTEL_PROJECT_ID`, `SELECTEL_USER`, `SELECTEL_PASSWORD` (a service user in the
  project; `SELECTEL_REGION` if a node does not set `region`)
- Fly: `FLY_API_TOKEN` (an app-scoped deploy token for the controlled app)
- headscale (tailnet status line): `HEADSCALE_API_KEY`, and `HEADSCALE_LOGIN_SERVER` in `fly.toml`
- the tailscale auth key for this app itself: `TAILSCALE_AUTH_KEY` (see `start.sh`)

## API

`GET /` (page) · `GET /nodes.json` (UI view: labels and status vocabulary, no connection details) ·
`GET /status` · `GET /nodes/{id}/status` · `POST /nodes/{id}/start` · `POST /nodes/{id}/stop` ·
`GET /tailnet/status` · `GET /healthz`. Start/stop are idempotent. A node whose settings are missing reports
`not_configured`, and a provider API failure shows as `error` on that node only (the others still render).

## Tailscale state

`fly.toml` mounts a volume at `/var/lib/tailscale` so the node keeps its identity across deploys. Create it once
(`flyctl volumes create tailscale_state -a <app> -r <region> -n 1`); without it every deploy registers a new
node in headscale.

## Enrolment (optional)

Setting `ENROLL_DB` turns on user enrolment for a REALITY ingress proxy. `start.sh` then runs a second, public
enrolment listener next to the admin app. Both bind to the tailscale IP only:

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
  `node-control.<tailnet-domain>,100.64.0.X`. Any other Host gets `400` (Starlette `TrustedHostMiddleware`, applied
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

## Tests

`python3 -m unittest discover -s tests` from this directory (no cloud access or SDKs needed).

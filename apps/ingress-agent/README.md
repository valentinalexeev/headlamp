# ingress-agent (the bridge agent)

A small service that runs next to xray on the Reality bridge host. It holds **only opaque
credentials** (`uuid`, `mode`, `not_after`), never names or owners. A compromised bridge host
does not learn who the clients are. The operator side (the enrolment code of node-control) pushes the
full list of valid credentials over an API that is only on the tailnet. The agent renders the xray config from a
base file. It restarts xray when the set changes.

> **HARD GATE before you enrol the first phone:** harden the headscale ACL first (see
> `ansible/roles/reality-ingress/README.md`). The routing block below acts on addresses that xray
> resolves. The `reality-ingress` role has an nftables backstop (`ingress-guard.service`).
> The backstop drops loopback, LAN and tailnet packets from the xray user in the kernel, whatever the name resolution gives.
> Neither of these replaces the ACL.

## API (bearer token in `Authorization: Bearer <AGENT_TOKEN>`; wrong or missing -> `401`, empty body)

- `PUT /v1/credentials` body `{"credentials":[{"uuid":"<uuid>","mode":"auto|direct|exit","not_after":<unix seconds>}]}`
  replaces the whole set. The status is `422` for a bad mode, a non-UUID string or a non-integer `not_after`.
  Duplicate UUIDs in one push give `422`. The agent drops entries that already expired first. Then an empty
  list gives `409`, unless the request has `?confirm_empty=true`.
  The status is `503` if xray rejects the generated config. In that case, the old config, the old set and the running xray do not change.
- `GET /v1/stats` -> `{"<opaque-id>":{"up":<bytes>,"down":<bytes>}}`. The opaque id is the first 8
  hex chars of `SHA-256(<uuid>)`. The operator side computes the same value from the `auto`
  UUID of a device. Stats never contain a UUID.

- `GET /v1/info` -> `{"name","host","port","sni","public_key","short_id"}`: the PUBLIC parameters that a client needs to
  reach this bridge (see "Several bridges and discovery"). The status is `503` with an empty body when `INGRESS_NAME`,
  `INGRESS_HOST`, `INGRESS_PUBLIC_KEY` or `INGRESS_SHORT_ID` is unset or empty. An older deployment keeps working for
  everything else. The response never contains the Reality private key, the agent token or any credential.

No other routes exist (docs and OpenAPI are disabled).

## What it writes into the xray config

The agent starts from the base file (`XRAY_BASE_CONFIG`). The base file must contain an inbound tagged `reality-in`, a
`routing` section and outbounds `direct`, `blocked`, `via-exit`. The agent writes these items:

- one client per credential on `reality-in` (`flow: xtls-rprx-vision`, email `u<12 hex of sha256(uuid)>@i`);
- `routing.domainStrategy` is set to `IPOnDemand`. This overrides the base value. Xray must resolve domain destinations
  before the block rule. If it does not, a hostname that points to a private address bypasses the rule.
- Routing rules, put before the base rules, in this order:
  1. Block `geoip:private` and `100.64.0.0/10` for everyone on the `reality-in` inbound. Phone
     users then cannot reach the LAN of the bridge host or the tailnet. This rule is present even with zero clients.
  2. Send clients of mode `direct` to the outbound `direct`. Send clients of mode `exit` to the outbound `via-exit`.
  3. Send clients of mode `auto` to `direct` for destinations on the configured direct lists.
     These lists are `INGRESS_DIRECT_GEOIP` and `INGRESS_DIRECT_GEOSITE` (comma-separated geodata list names, empty by default).
     Send clients of mode `auto` to `via-exit` for all other destinations.

  The base rules follow, unchanged.

The agent writes the new config next to `XRAY_CONFIG` and checks it with `xray run -test`. It then replaces the config atomically
and restarts the unit. The agent saves the set only after a successful restart. A failed restart gives `503`, and a retry applies the set again.

An unchanged push does nothing. The agent runs one apply at a time. On startup, the agent renders the
saved set again. It restarts xray only if the config on disk differs. Every 60 s,
the agent removes expired entries and applies the config again.

## Configuration (environment)

| variable | default |
|---|---|
| `AGENT_TOKEN` | required |
| `AGENT_STATE_DIR` | `/var/lib/ingress-agent` (holds `credentials.json`, mode 0600) |
| `XRAY_BASE_CONFIG` | `/etc/xray/config.base.json` |
| `XRAY_CONFIG` | `/etc/xray/config.json` |
| `XRAY_BIN` | `/usr/local/bin/xray` |
| `XRAY_UNIT` | `xray` |
| `XRAY_API` | `127.0.0.1:10085` (xray stats API, must be enabled in the base config) |
| `INGRESS_DIRECT_GEOIP` | empty. Comma-separated geodata list names; in `auto` mode, destinations on them leave from this host |
| `INGRESS_DIRECT_GEOSITE` | empty. Same, for geosite lists |
| `INGRESS_NAME` | unset. Short label of this bridge, `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`. Needed for `/v1/info` |
| `INGRESS_HOST` | unset. Public DNS name of the bridge (lowercase, no scheme, port or path). Needed for `/v1/info` |
| `INGRESS_PORT` | `443`. Public port, 1-65535 |
| `INGRESS_SNI` | `INGRESS_HOST`. Reality server name, same format as the host |
| `INGRESS_PUBLIC_KEY` | unset. Public half of the Reality key pair, no whitespace. Needed for `/v1/info` |
| `INGRESS_SHORT_ID` | unset. Reality short id, no whitespace. Needed for `/v1/info` |

The app validates the `INGRESS_*` values above when it starts. An invalid value stops the start with an error
that names the variable (never the value). There is no variable for the private key. Do not add one.

## Several bridges and discovery

`GET /v1/info` lets a newly deployed bridge tell node-control how clients reach it. The operator then does not need to
copy the host, public key and short id by hand. It publishes only public values. node-control reads it and shows the
values. The operator approves the bridge there. Nothing is used before that approval. Each bridge host has its own
`AGENT_TOKEN` (a token for each bridge host), so a leaked token opens one bridge only.

The agent is not a trust boundary against its own host. A person with root on a bridge controls its agent and xray.
That person sees the credentials that the operator pushes to it. For this reason, node-control derives a different credential for each bridge.
It also pins an approved bridge to its published parameters and its headscale node.
See "Compromised bridge" in the node-control README for what this does and does not protect.

## Run

Bind to the tailnet address only. Disable the access log, because it records client addresses:

    pip install -r requirements.txt
    AGENT_TOKEN=... uvicorn app.main:create_app_from_env --factory \
        --host <tailnet-ip> --port 9000 --no-access-log

Run the service as a user that can write `XRAY_CONFIG` and run `systemctl restart`.
Logs contain counts only (`applied: +2 -1`), never UUIDs, emails or the token.

## Tests

    python3 -m pip install fastapi httpx uvicorn
    python3 -m unittest discover -s tests

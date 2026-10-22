# ingress-agent

A small service that runs next to xray on the Reality ingress host. It holds **only opaque
credentials** (`uuid`, `mode`, `not_after`), never names or owners, so a compromised ingress host
does not learn who the clients are. The operator side (node-control's enrolment code) pushes the
full list of valid credentials over a tailnet-only API; the agent renders the xray config from a
base file and restarts xray when the set changes.

> **HARD GATE before the first phone is enrolled:** harden the headscale ACL first (see
> `ansible/roles/reality-ingress/README.md`). The routing block below acts on addresses xray
> resolves; the `reality-ingress` role's nftables backstop (`ingress-guard.service`) drops
> loopback/LAN/tailnet packets from the xray user in the kernel regardless of name resolution.
> Neither replaces the ACL.

## API (bearer token in `Authorization: Bearer <AGENT_TOKEN>`; wrong or missing -> `401`, empty body)

- `PUT /v1/credentials` body `{"credentials":[{"uuid":"<uuid>","mode":"auto|direct|exit","not_after":<unix seconds>}]}`
  replaces the whole set. `422` for a bad mode, a non-UUID string or a non-integer `not_after`.
  Duplicate UUIDs in one push are `422`. Entries already expired are dropped first, then `409` for an empty
  list unless `?confirm_empty=true`.
  `503` if xray rejects the generated config (old config, old set and the running xray are untouched).
- `GET /v1/stats` -> `{"<opaque-id>":{"up":<bytes>,"down":<bytes>}}`. The opaque id is the first 8
  hex chars of `SHA-256(<uuid>)`, the same value the operator side computes from a device's `auto`
  UUID. Stats never contain a UUID.

No other routes exist (docs and OpenAPI are disabled).

## What it writes into the xray config

From the base file (`XRAY_BASE_CONFIG`), which must contain an inbound tagged `reality-in`, a
`routing` section and outbounds `direct`, `blocked`, `via-exit`:

- one client per credential on `reality-in` (`flow: xtls-rprx-vision`, email `u<12 hex of sha256(uuid)>@i`);
- `routing.domainStrategy` is forced to `IPOnDemand` (the base value is overridden: domain destinations must
  be resolved before the block rule, or a hostname pointing at a private address would bypass it);
- routing rules prepended, in order: block `geoip:private` and `100.64.0.0/10` for everyone on the `reality-in` inbound (so phone
  users cannot reach the ingress host's LAN or the tailnet; present even with zero clients), then
  `direct` -> `direct`, `exit` -> `via-exit`, and `auto` -> `direct` for destinations on the configured direct lists
  (`INGRESS_DIRECT_GEOIP` / `INGRESS_DIRECT_GEOSITE`, comma-separated geodata list names; empty by default) and `via-exit`
  for everything else; then the base rules unchanged.

The new config is written next to `XRAY_CONFIG`, checked with `xray run -test`, atomically replaced,
then the unit is restarted; the set is persisted only after a successful restart (a failed restart gives `503` and a retry re-applies). An unchanged push does nothing. Applies are serialised. On startup the
persisted set is re-rendered (xray is restarted only if the config on disk differs). Every 60 s,
expired entries are swept and the config re-applied.

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

## Run

Bind to the tailnet address only, and disable the access log (it would record client addresses):

    pip install -r requirements.txt
    AGENT_TOKEN=... uvicorn app.main:create_app_from_env --factory \
        --host <tailnet-ip> --port 9000 --no-access-log

The service must run as a user allowed to write `XRAY_CONFIG` and to run `systemctl restart`.
Logs contain counts only (`applied: +2 -1`), never UUIDs, emails or the token.

## Tests

    python3 -m pip install fastapi httpx uvicorn
    python3 -m unittest discover -s tests

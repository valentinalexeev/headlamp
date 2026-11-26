# ingress-agent (the bridge agent)

A small Go service (one static binary, no runtime) that runs next to xray on the Reality bridge host. It holds **only opaque
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
  replaces the whole set. An entry can also have `"tailnet"`: `"all"` (the tailnet grant, #132) or a grant group id
  `"g-<8 hex>"` (#139). The status is `422` for a bad mode, a non-UUID string, a non-integer `not_after` or another
  `tailnet` value. The answer is `{"count": <n>, "tailnet_groups": [<group ids>]}`. `tailnet_groups` lists the groups
  that have an outbound in the base config.
  Duplicate UUIDs in one push give `422`. The agent drops entries that already expired first. Then an empty
  list gives `409`, unless the request has `?confirm_empty=true`.
  The body can also have `"tailnet_dns": {"server": "tcp://<tailnet ipv4>:<port>", "domains": [...]}` (#286). node-control
  sends it to a bridge with tailnet access. `server` must be a tailnet IPv4 address. Each entry of `domains` is
  `full:<name>` or `domain:<name>`, 1,024 entries or fewer. Another value gives `422`. A push without the key removes
  the resolver. The agent keeps the resolver in `tailnet-dns.json`, next to `credentials.json`.

  The status is `503` if xray rejects the generated config. In that case, the old config, the old set and the running xray do not change.
- `GET /v1/stats` -> `{"<opaque-id>":{"up":<bytes>,"down":<bytes>}}`. The opaque id is the first 8
  hex chars of `SHA-256(<uuid>)`. The operator side computes the same value from the `auto`
  UUID of a device. Stats never contain a UUID.

- `GET /v1/info` -> `{"name","host","port","sni","public_key","short_id"}`: the PUBLIC parameters that a client needs to
  reach this bridge (see "Several bridges and discovery"). The status is `503` with an empty body when `INGRESS_NAME`,
  `INGRESS_HOST`, `INGRESS_PUBLIC_KEY` or `INGRESS_SHORT_ID` is unset or empty. An older deployment keeps working for
  everything else. The response never contains the Reality private key, the agent token or any credential.
  With `INGRESS_HY2_PORT` it also has `hy2_port`. The Salamander password is a secret and is never in it.

- `GET /v1/egress` -> the bridge path self-test (#196): `{"ok", "checked", "since", "error", "duration_ms", "status",
  "target"}`. Every minute the agent requests `INGRESS_EGRESS_URL` through the SOCKS port of the exit node (the
  `via-exit` outbound). The exit node resolves the host name, as for clients. `ok` is false when that fails.
  Then clients connect and get no reply, though every component looks healthy. Common causes are an offline or
  replaced exit node, or a policy without `autogroup:internet` for `tag:ingress` (#259). Without
  `INGRESS_EGRESS_SOCKS` the answer is `{"disabled": true}`. node-control shows a failed self-test as an Attention item.

- `GET /v1/pathcheck` -> the path check from this bridge to the control host (see "Path check"). Without
  `INGRESS_PATHCHECK_TARGET` the answer is `{"disabled": true}`. These are the fields of the answer:

  | Field | Meaning |
  |---|---|
  | `class` | The class of the last run |
  | `since`, `streak` | When that class started, and the number of runs in a row with it |
  | `checked`, `interval_s` | The time of the last run, and the interval |
  | `counts`, `history` | The classes of the last 96 runs, as counts and as a short list |
  | `bytes_today` | The transfer bytes of this UTC day |
  | `last` | All probe results of the last run |

  The answer has no address, URL or server message. Errors are short classes, for example `timeout` or `reset`.

Hysteria 2 (#110): the base config can have an inbound with the tag `hy2-in` and the protocol `hysteria`. The agent
then adds every credential to it as well. The uuid is the Hysteria `auth` value, and the email is the same as on
`reality-in`. Thus the per-user routing rules, the block rule and the statistics apply to both inbounds.

No other routes exist (docs and OpenAPI are disabled).

## What it writes into the xray config

The agent starts from the base file (`XRAY_BASE_CONFIG`). The base file must contain an inbound tagged `reality-in`, a
`routing` section and outbounds `direct`, `blocked`, `via-exit`. The agent writes these items:

- one client per credential on `reality-in` (`flow: xtls-rprx-vision`, email `u<12 hex of sha256(uuid)>@i`);
- `routing.domainStrategy` is set to `IPOnDemand`. This overrides the base value. Xray must resolve domain destinations
  before the block rule. If it does not, a hostname that points to a private address bypasses the rule.
- the `dns` section. This replaces the base value. Xray uses it for each lookup of `IPOnDemand`. The servers are
  `INGRESS_EXIT_DNS` (default `https://1.1.1.1/dns-query`, `https://1.0.0.1/dns-query`). The tag of the section is
  `dns-internal`, and a routing rule sends these queries to `via-exit`. Thus the resolver of the bridge network does
  not see the names that clients go to. Only names on `INGRESS_DIRECT_GEOSITE` use the system resolver (server
  `localhost` with `skipFallback`), because their traffic leaves from this host.

  With a tailnet resolver from node-control (#286), the first server is node-control on the tailnet, for the names in
  `domains` only. It has `skipFallback` and `finalQuery`: the names go to node-control only, and other names never go
  to it. `expectedIPs` accepts only tailnet addresses. The `dns-internal` rule sends the query to `via-exit`. The
  tailscaled of the exit node sends a tailnet address over the tailnet.

  The name then resolves to a tailnet address. Rule 0 below sends the traffic of a granted client to the tailnet. The
  block rule stops the traffic of other clients.

  **CAUTION:** Clients of mode `direct` also get their addresses from the exit DNS servers. When the exit node is
  not available, their lookups fail.
- Routing rules, put before the base rules, in this order:
  - Send the queries of the `dns-internal` tag to `via-exit`. The base config must have this outbound, or the push
    gives `503`.
  0. This rule exists only when a credential has the tailnet grant (#132). It sends the tailnet ranges of these
     clients to the outbound `via-tailnet`. The ranges are `100.64.0.0/10` and `fd7a:115c:a1e0::/48`. The base config must then have this outbound, or the push gives
     `503`. Other private ranges are not in this rule, so they stay blocked.
     For each grant group (#139), one more rule sends the tailnet ranges of the group's clients to the outbound
     `via-tailnet-<group id>`. If the base config has no outbound for a group, the group gets no rule. Thus the block
     rule stops its tailnet traffic until the bridge has the SOCKS instance of the group. A playbook run can add an
     outbound. Then the next push renders the config again, also if the credentials did not change.
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
| `INGRESS_EGRESS_SOCKS` | empty: no self-test. `host:port` of the SOCKS port of the exit node (the `socks_listen` of `reality-ingress`, `127.0.0.1:1055`). The role sets it by default (#196) |
| `INGRESS_EGRESS_URL` | empty: `https://www.gstatic.com/generate_204`. The `https` URL of the self-test. The agent sends one `HEAD` request a minute |
| `INGRESS_EXIT_DNS` | empty: `https://1.1.1.1/dns-query,https://1.0.0.1/dns-query`. Comma-separated resolvers for the lookups of xray, through `via-exit`. Each is `https://<ipv4>/<path>` or `tcp://<ipv4>[:port]`. A name is not accepted, because its lookup leaves through the local network |
| `INGRESS_NAME` | unset. Short label of this bridge, `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`. Needed for `/v1/info` |
| `INGRESS_HOST` | unset. Public DNS name of the bridge (lowercase, no scheme, port or path). Needed for `/v1/info` |
| `INGRESS_PORT` | `443`. Public port, 1-65535 |
| `INGRESS_SNI` | `INGRESS_HOST`. Reality server name, same format as the host |
| `INGRESS_PUBLIC_KEY` | unset. Public half of the Reality key pair, no whitespace. Needed for `/v1/info` |
| `INGRESS_SHORT_ID` | unset. Reality short id, no whitespace. Needed for `/v1/info` |
| `INGRESS_HY2_PORT` | unset. Public UDP port of the Hysteria 2 inbound, 1-65535. When set, `/v1/info` has `hy2_port` |
| `INGRESS_PATHCHECK_TARGET` | empty: no path check. `host[:port]` of the control host (the host of headscale `server_url`). Port default `443` |
| `INGRESS_PATHCHECK_ADDRS` | empty. Comma-separated IP addresses. When set, the name must resolve to one of them, else the class is `dns-tampered` |
| `INGRESS_PATHCHECK_HEALTH_PATH` | `/health`. The headscale health path. `-`: no HTTP probe |
| `INGRESS_PATHCHECK_DERP_PATH` | `/derp`. The DERP path of the embedded DERP server. `-`: no DERP probe |
| `INGRESS_PATHCHECK_STUN` | empty: `<target host>:3478`. `host:port` of STUN. `-`: no UDP probe |
| `INGRESS_PATHCHECK_BLOB_URL` | empty: no transfer. The `https` URL of a file on the central side, at least `INGRESS_PATHCHECK_BLOB_BYTES` long |
| `INGRESS_PATHCHECK_BLOB_BYTES` | `1048576`. Bytes for each transfer, 1 to 16777216 |
| `INGRESS_PATHCHECK_REF_URL` | empty: no reference. The `https` URL of a neutral host. It must serve a file for the transfer too, if `INGRESS_PATHCHECK_BLOB_URL` is set |
| `INGRESS_PATHCHECK_INTERVAL` | `15m`. Go duration, at least `1m`. Each wait is 80% to 120% of it |
| `INGRESS_PATHCHECK_BLOB_EVERY` | `4`. The transfer runs on every n-th check |
| `INGRESS_PATHCHECK_DAILY_BYTES` | `52428800` (50 MiB). The transfer budget for each UTC day. When it is used, the transfer does not run |
| `INGRESS_PATHCHECK_TAILSCALE` | empty: not read. Absolute path of the `tailscale` CLI, for the passive probe |
| `INGRESS_PATHCHECK_TAILSCALE_SOCKET` | empty: the default socket of tailscaled |
| `XRAY_RESTART_CMD` | unset. A command that replaces `systemctl restart <XRAY_UNIT>`, for a host without systemd |
| `AGENT_LISTEN` | `127.0.0.1:9000`. The address to listen on. The Ansible wrapper sets the host's tailnet IPv4 |

The app validates the `INGRESS_*` values above when it starts. An invalid value stops the start with an error
that names the variable (never the value). There is no variable for the private key. Do not add one.

## Path check

The path check tests the path from the network of this bridge to the control host. It does not use the client
tunnel or the exit node. Its results show if a network between the bridge and the control host blocks or slows the
traffic of the tailnet. Use them to decide if that traffic must be wrapped in an obfuscated tunnel.

The check is off by default. Set `INGRESS_PATHCHECK_TARGET` to turn it on. It opens no port and changes no access rule.
It sends only small requests to the control host, plus a request to the reference host when you set one.

Each run does these probes in sequence. A probe that needs an earlier probe does not run when that probe fails.

| Probe | What it does | What a failure shows |
|---|---|---|
| `dns` | Resolves the name of the control host | the name does not resolve, or resolves to an address not in `INGRESS_PATHCHECK_ADDRS` |
| `tcp` | Opens a TCP connection to the first address | the address is blocked |
| `tls` | Does a TLS handshake with the real server name | with `tls_no_sni`: a block by name or a block of TLS |
| `tls_no_sni` | Sends a TLS hello with no server name to the same address. A TLS alert counts as an answer | the address does not answer at the TLS layer |
| `http` | Sends `GET` on the health path and expects `200` | a block of the application layer |
| `derp` | Opens the DERP upgrade and reads the first frame of the server | DERP is blocked while HTTPS works |
| `stun` | Sends a STUN binding request over UDP, two times | UDP is blocked |
| `ref` | Sends `HEAD` to the reference URL | the uplink of the bridge itself has a problem |
| `blob`, `ref_blob` | Reads `INGRESS_PATHCHECK_BLOB_BYTES` from each URL, and records the rate, the time to the first byte and the longest gap | a transfer that stops (no data for 10 s), or a rate below a quarter of the reference rate |
| `tailscale` | Reads `tailscale status --json`: the state, the number of health warnings, and how tailscaled reaches the exit node | tailscaled is not running, or it reaches the exit node only through DERP |

The run gets one class: the first finding in this list, or `ok`. `findings` has all the findings of the run.

| Class | Meaning |
|---|---|
| `edge-uplink-bad` | The reference host and the control host both fail. The problem is the uplink of the bridge. |
| `dns-failed`, `dns-tampered` | The name does not resolve, or it resolves to an address that is not expected. |
| `ip-blocked` | No TCP connection to the control host. |
| `sni-blocked` | TLS with the name fails, and TLS without the name gets an answer. |
| `tls-blocked` | TLS fails with and without the name. |
| `app-blocked`, `derp-blocked` | TLS works, but the health request or the DERP upgrade fails. |
| `tailnet-down` | The tailscaled of the bridge is not running. |
| `stalled`, `transfer-failed` | The transfer stops after it starts, or fails in another way. |
| `throttled` | The transfer from the control host is more than four times slower than from the reference host. |
| `udp-blocked` | STUN gets no answer and TCP works. |
| `derp-only` | tailscaled reaches the exit node only through DERP. |

The agent keeps the last 96 runs (24 hours at the default interval) in `pathcheck.json` in `AGENT_STATE_DIR`, mode
`0600`. A restart keeps them. node-control shows the result on the overview (see its README).

For the transfer, put a file of random data on a host of the central side. Use a path that is difficult to guess.
For example, add this route to the TLS front of the control host:

    handle /<random path>/probe.bin {
        root * /srv/probe
        file_server
    }

Create the file with `head -c 1048576 /dev/urandom > /srv/probe/<random path>/probe.bin`. The reference host must
also serve a file of the same size or larger. The transfer uses a `Range` request, so a larger file is not read fully.

The compose example of the control host has this route. Set `PATHCHECK_BLOB_PATH` to turn it on. See
`examples/compose/control/README.md`, "Path check test file".

> **CAUTION:** Keep the daily budget low on a metered uplink. Each transfer reads `INGRESS_PATHCHECK_BLOB_BYTES` from
> the control host, and the same amount from the reference host.

The probes are regular traffic of a client of the control host, with a jitter of the interval. They do not try to
hide. A network that sees the traffic of the bridge to the control host already sees the same hosts.

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

Bind to the tailnet address only. The agent keeps no access log:

    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ingress-agent ./cmd/ingress-agent
    AGENT_TOKEN=... AGENT_LISTEN=<tailnet-ip>:9000 ./ingress-agent

Run the service as a user that can write `XRAY_CONFIG` and run `systemctl restart`.
Logs contain counts only (`applied: +2 -1`), never UUIDs, emails or the token.

## Resources (#133)

The agent was a Python service (FastAPI and uvicorn) until 0.11.0. The Go port has the same API, the same
environment and the same files, so node-control and a running xray see no change. Measured on Linux with
`scripts/test/ingress-agent-budget/budget.sh`:

| | Python (before) | Go (now) | Budget |
|---|---|---|---|
| Idle memory (RSS) | about 52 MB | about 12 to 14 MB | 15 MB or less |
| Memory with 1,000 credentials | not measured | about 16 to 18 MB | 20 MB or less |
| Start to the first `/v1/info` answer | not measured | about 60 ms | less than 200 ms |
| On disk | about 79 MB (Python and packages) | about 6 MB (one binary) | |

## Tests

    go test -race ./...
    sh ../../scripts/test/ingress-agent-budget/budget.sh   # the resource budget, in Docker

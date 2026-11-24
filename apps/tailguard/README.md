# apps/tailguard — WireGuard server on the host, bridged to a Tailscale exit node

[TailGuard](https://github.com/juhovh/tailguard) runs in a Docker container on
the host. A plain WireGuard client (a home router) connects to it. All traffic of the client
leaves through a Tailscale exit node.

It works against the control plane of Tailscale or a headscale server (see the end of this
file). The examples below call the exit node `my-exit`.

This is an **operator-run** unit (like `apps/*`). Ansible and Terraform do not
manage it. Key material lives only on the host, never in the repo (`.env`,
`config/wg0.conf`, and `state/` are gitignored).

TailGuard runs its **own** in-container Tailscale as a separate node
(`tg-node`). The Tailscale node of the host, if it has one, does not change.

## Layout

| file | committed | purpose |
|---|---|---|
| `docker-compose.yml`     | yes | the `tailguard` container + `route-health` sidecar |
| `.env.example`           | yes | documented knobs |
| `.env`                   | no  | the site settings; `TS_AUTHKEY` only until the first login |
| `config/wg0.conf.example`| yes | server-side WG template |
| `config/wg0.conf`        | no  | real server privkey + client peer(s) |
| `state/`                 | no  | in-container tailscale state |
| `verify.sh`              | yes | egress-proof test (throwaway in-tunnel WG client) |
| `route-health.sh`        | yes | watchdog: closes WG when the exit node stops forwarding |
| `track-exit-node.sh`     | yes | sidecar: re-selects the exit node when my-exit re-registers (new IP / suffixed name) |
| `run/`                   | no  | tailscaled socket dir, bind-mounted at `/tmp` in tailguard + tracker |

## exit-node-tracker

my-exit re-registers from time to time with a new Tailscale IP (and possibly a
suffixed name), so `.env` does **not pin the exit node**. The
`exit-node-tracker` sidecar talks to the tailscaled of tailguard through the
shared `./run:/tmp` socket. Every 20 s it does this:

- If the selected exit node is online, the sidecar leaves it alone.
- If not, the sidecar selects the online exit node whose name starts with
  `EXIT_NODE_PREFIX` and that has the tag `EXIT_NODE_TAG` (default `tag:exit`). `EXIT_NODE_PREFIX` has no
  default, so set it. A prefix such as `my-exit` also matches `my-exit-1`.
- If the selected exit node does not have the tag, the sidecar selects a different node. A node that uses the
  name of an exit node without the tag does not get the traffic.
- `TG_LAN_ACCESS` sets the LAN access through the exit node. `0` sends the private address ranges to the exit
  node too, so a WireGuard peer cannot reach the LAN of the host. `1` lets a WireGuard peer reach the LAN of
  the host. The variable has no default. If it is not set, the sidecar stops with a message.

Compose blanks `TS_EXIT_NODE` for tailguard on purpose. The environment is fixed at container
creation, so a stale pin would crash `tailscale up` after a reboot. After each boot, there is a
short time when the exit node is not selected yet. Egress goes
direct until the tracker pins the node (~30 s). To see the logs, run `docker logs -f
tailguard-exit-node-tracker`.

The tracker also has a **derp-guard** (`BLOCK_DERP_REGIONS`, empty by default). On
some uplinks, a DERP region answers STUN (UDP/3478) but drops its TCP/443. Tailscaled then picks
it as home relay by UDP latency and never connects. Every reply
from peers is lost.

The exit-node `tx` grows, `rx` stays 0, and the `tailscale status` health
says "could not connect to the '<region>' relay". The derp-guard drops our STUN to those regions, so tailscaled uses a
reachable region as home. An alternative fixes this for every node: remove the region
in the tailnet policy, `"derpMap": {"Regions": {"<region-id>": null}}`. To self-test the decision logic, run
`printf '100.1.1.1 my-exit-1 1 0 1\n' | EXIT_NODE_PREFIX=my-exit sh track-exit-node.sh --decide`. The columns are
the IP, the name, online, selected and tagged (`0` or `1`).

## route-health watchdog

The `route-health` service is a sidecar that shares the network namespace of `tailguard`.
Every 12 s it probes real internet reachability **through the exit node**. After 3
consecutive failures (~90 s on a black-holed node), it runs
`iptables -I INPUT 1 -p udp --dport 51820 -j DROP` in the namespace of tailguard. WG clients
then see the tunnel die and fail over to their own WAN.

After 2
consecutive successes (~24 s), it removes the rule. It restores the listener on
container stop (fail-open). To watch it, run `docker logs -f tailguard-route-health`.
To tune it, set `FAIL_N`, `OK_N`, `INTERVAL` and `PROBE_TIMEOUT` under `environment:` in
`docker-compose.yml`.

Fault-injection test (simulate my-exit not forwarding):

```bash
docker exec tailguard iptables  -I OUTPUT 1 -o tailscale0 -j DROP
docker exec tailguard ip6tables -I OUTPUT 1 -o tailscale0 -j DROP
# ~90s later: log shows "EGRESS DOWN ... closing wg udp/51820"
docker exec tailguard iptables  -D OUTPUT -o tailscale0 -j DROP
docker exec tailguard ip6tables -D OUTPUT -o tailscale0 -j DROP
# ~24s later: "EGRESS OK ... reopening wg udp/51820"
```

NOTE: This test closes the WG *server* listener. LAN clients still need their own
failover (a WAN route, mwan3 or a watchdog) to use the internet while the
tunnel is down. Without it, there is no traffic until my-exit returns.

## Deploy (on the host)

### 0. Prereqs

- The LAN interface of the host has a **static DHCP reservation** (`<host-lan-ip>`). The
  WireGuard peer of the router uses it as its Endpoint.
- Get a **one-time** (not reusable) pre-auth key with the tag `tag:tailguard`. The node then joins as a
  tagged node, with no user ownership.
- In `tagOwners` of the tailnet policy, give `tag:tailguard` only to the admin or service identities (see
  `apps/headscale/acl.json`).
- In the tailnet policy, give `tag:tailguard` the internet through the exit node and no tailnet
  destinations:
  `{"action": "accept", "src": ["tag:tailguard"], "dst": ["autogroup:internet:*"]}`
  (see `examples/compose/control/config/acl.json`).

**WARNING:** Do not use an untagged key that your user owns. The node then gets all the access of that user, and
each WireGuard peer can reach the same tailnet addresses.

### 1. Get this directory onto the host

Put `apps/tailguard/` at `~/apps-tailguard` (sparse checkout or `scp -r`), then
`cd ~/apps-tailguard`.

### 2. Keys and config

```bash
cd config
docker run --rm --entrypoint wg ghcr.io/juhovh/tailguard:latest genkey | tee srv.key | \
  docker run --rm -i --entrypoint wg ghcr.io/juhovh/tailguard:latest pubkey > srv.pub
docker run --rm --entrypoint wg ghcr.io/juhovh/tailguard:latest genkey | tee cli.key | \
  docker run --rm -i --entrypoint wg ghcr.io/juhovh/tailguard:latest pubkey > cli.pub
docker run --rm --entrypoint wg ghcr.io/juhovh/tailguard:latest genpsk > psk.key
cp wg0.conf.example wg0.conf
# edit wg0.conf: PrivateKey=<srv.key>, [Peer] PublicKey=<cli.pub>, PresharedKey=<psk.key>
chmod 600 wg0.conf
cd ..
cp .env.example .env
# edit .env: set TS_AUTHKEY, TG_LAN_ACCESS, TG_STATUS_PORT and TG_BIND_ADDR
chmod 600 .env
```

`.env` must set these variables. If a variable is missing, `docker compose up` stops with a message.

| variable | value | result |
|---|---|---|
| `TG_LAN_ACCESS` | `0` (recommended) | WireGuard peers reach the internet through the exit node only. |
| | `1` | WireGuard peers can also reach the LAN of the host. |
| `TG_STATUS_PORT` | empty (recommended) | The status page is off. |
| | `<port>`, for example `8088` | The status page is on, on `wg0` and `tailscale0`. It replaces `TG_WEBUI_PORT`. |
| `TG_BIND_ADDR` | `<host-lan-ip>` (recommended) | The WireGuard and tailscale ports listen on that host address only. |
| | `0.0.0.0` | The ports listen on all host addresses. |

### 3. Up

```bash
docker compose up -d && docker compose logs -f
```

Expect: `wg0` up -> firewall rules -> `containerboot` -> tailscale auth OK ->
healthcheck green -> delayed script sets the exit node.

### 4. Remove the auth key

1. Make sure that `state/` contains the node state (`docker compose exec tailguard tailscale status` shows the node).
2. Remove the `TS_AUTHKEY` line from `.env`.
3. Run `docker compose up -d` to create the container again without the key.

## Verify (host only, no router yet)

```bash
# tg-node is registered, my-exit is the active exit node
docker compose exec tailguard tailscale status

# on the headscale server: tg-node has tag:tailguard and no user
headscale nodes list

# read-only status page, only with TG_STATUS_PORT=8088 and a policy rule that lets your node reach it:
#   curl http://<tg-node-tailscale-ip>:8088
```

To prove egress through the WG path, run `./verify.sh`. It runs a throwaway WireGuard
client (from `config/cli-test.conf`, built from the client template below). The client
pushes a download through the tunnel and reads the exit-node byte counters
before and after:

```bash
./verify.sh
```

Expect: `rx` of the `my-exit` peer grows by roughly the download size. This means that the traffic of the WG
client went through the exit node.

**Do not use the public egress IP as the signal if the host and the exit node
share an egress address** (same cloud or NAT). `curl ifconfig.co` then returns the same IP
with or without the tunnel. Use the peer counters instead (`tailscale status` `tx/rx` on the
exit-node line), or a difference in `colo=` of Cloudflare `cdn-cgi/trace`.

Two gotchas apply when you run a throwaway client with `--entrypoint sh` (this bypasses
`entrypoint.sh`):

- Run `resolvconf -u` first. If you do not, `resolvconf -a` of `wg-quick`
  fails with "signature mismatch".
- The `DNS =` line of the client config triggers that call.

## Home-router client config

```ini
[Interface]
PrivateKey = <contents of config/cli.key>
Address = 10.99.0.2/32, fd99:7461:6c67::2/128
DNS = 1.1.1.1
MTU = 1280

[Peer]
PublicKey = <contents of config/srv.pub>
PresharedKey = <contents of config/psk.key>
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = <host-lan-ip>:51820
PersistentKeepalive = 25
```

- `AllowedIPs = 0.0.0.0/0, ::/0`: everything egresses through the exit node.
- `DNS = 1.1.1.1` goes over the tunnel (the client is not a Tailscale node). To get
  tailnet name resolution, use `100.100.100.100` instead.
- `MTU = 1280` matches `tailscale0`. It avoids fragmentation in the nested
  tunnels.
- The router must keep its direct route to `<lan-subnet>` (default). Then the
  handshake to `.101` does not go into the tunnel.

After you load the config into the router, run the `ifconfig.co` check again from a LAN
client behind the router. Make sure that DNS resolves and does not leak.

**Rollback:** Run `docker compose down` here and disable the router tunnel. Nothing on
the host changes. If you want, delete `tg-node` from the Tailscale console.

## Troubleshooting

| symptom | cause / fix |
|---|---|
| `cannot resolve exit node by hostname while Tailscale is starting up` | `TS_EXIT_NODE` is a hostname — use the IP (`100.x.y.z`) or `auto:any`. |
| `failed to auth tailscale ... tailscale up failed` | `TS_AUTHKEY` missing, expired or already used, or `TS_LOGIN_SERVER` wrong. A one-time key works once: for a new login, make a new key. |
| `docker compose up` stops with `... is not set in .env (#390)` | Set the variable that the message names. See the table in "2. Keys and config". |
| tracker logs `waiting: no-online-exit-node-matching-...-with-tag:exit` | The exit node does not have `EXIT_NODE_TAG`. Give it the tag, or set `EXIT_NODE_TAG`. |
| container won't bind 41642 | something else took it — pick another `TS_PORT` and update the compose port. |
| exit node not applied | check `docker compose exec tailguard tailscale status`; the exit node is re-set by a delayed script once the healthcheck passes — give it ~1 min. |
| poor throughput | nested-tunnel MTU — confirm client `MTU=1280`; check `tailscale status` for a DERP vs direct path to my-exit. |
| container replaced unexpectedly | watchtower auto-updated `:latest`; set the `watchtower.enable=false` label in `docker-compose.yml`. |

## Using headscale instead of Tailscale's own control plane

1. In `.env`, set `TS_LOGIN_SERVER=https://<your-headscale-url>`.
   Replace `TS_AUTHKEY` with a one-time headscale pre-auth key with the tag `tag:tailguard`. Run
   `headscale preauthkeys create --tags tag:tailguard` (check `--help`: the flags differ between versions).
   Set `EXIT_NODE_PREFIX` to the name of one of your headscale exit nodes.
2. Run `rm -rf state/ && docker compose up -d` to authenticate again against the new control
   server. `config/wg0.conf` and the router config do not change.

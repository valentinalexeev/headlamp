# apps/tailguard — WireGuard server on the host, bridged to a Tailscale exit node

[TailGuard](https://github.com/juhovh/tailguard) in a Docker container on
the host: a plain WireGuard client (a home router) connects, and all its
traffic egresses through a Tailscale exit node.

It works against Tailscale's own control plane or a headscale server (see the end of this
file). The examples below call the exit node `my-exit`.

This is an **operator-run** unit (like `apps/fly/*`). Not Ansible/Terraform
managed. Key material lives only on the host, never in the repo (`.env`,
`config/wg0.conf`, and `state/` are gitignored).

TailGuard runs its **own** in-container Tailscale as a separate node
(`tg-node`). The host's own Tailscale node, if it has one, is untouched.

## Layout

| file | committed | purpose |
|---|---|---|
| `docker-compose.yml`     | yes | the `tailguard` container + `route-health` sidecar |
| `.env.example`           | yes | documented knobs |
| `.env`                   | no  | real `TS_AUTHKEY`, exit-node IP |
| `config/wg0.conf.example`| yes | server-side WG template |
| `config/wg0.conf`        | no  | real server privkey + client peer(s) |
| `state/`                 | no  | in-container tailscale state |
| `verify.sh`              | yes | egress-proof test (throwaway in-tunnel WG client) |
| `route-health.sh`        | yes | watchdog: closes WG when the exit node stops forwarding |
| `track-exit-node.sh`     | yes | sidecar: re-selects the exit node when my-exit re-registers (new IP / suffixed name) |
| `run/`                   | no  | tailscaled socket dir, bind-mounted at `/tmp` in tailguard + tracker |

## exit-node-tracker

my-exit re-registers from time to time with a new Tailscale IP (and possibly a
suffixed name), so the exit node is **not pinned in `.env`**. The
`exit-node-tracker` sidecar talks to tailguard's own tailscaled through the
shared `./run:/tmp` socket and, every 20 s: leaves the selected exit node alone
if it is online; otherwise selects the online exit node whose name starts with
`EXIT_NODE_PREFIX` (no default — set it; a prefix such as `my-exit` also matches `my-exit-1`). Compose
blanks `TS_EXIT_NODE` for tailguard on purpose: env is baked in at container
creation, so a stale pin would crash `tailscale up` after a reboot. Expect a
short window after each boot where the exit node isn't selected yet (egress goes
direct until the tracker pins it, ~30 s). Logs: `docker logs -f
tailguard-exit-node-tracker`.

The tracker also has a **derp-guard** (`BLOCK_DERP_REGIONS`, empty by default): on
some uplinks a DERP region answers STUN (UDP/3478) but drops its TCP/443, so
tailscaled picks it as home relay by UDP latency, never connects, and every reply
from peers is lost (exit-node `tx` grows, `rx` stays 0, `tailscale status` health
says "could not connect to the '<region>' relay"). Dropping our STUN to those regions makes tailscaled home on a
reachable one. Alternative that fixes it for every node: remove the region
in the tailnet policy, `"derpMap": {"Regions": {"<region-id>": null}}`. Self-test of the decision logic:
`printf '100.1.1.1 my-exit-1 1 0\n' | EXIT_NODE_PREFIX=my-exit sh track-exit-node.sh --decide`.

## route-health watchdog

The `route-health` service (a sidecar sharing `tailguard`'s network namespace)
probes real internet reachability **through the exit node** every 12 s. After 3
consecutive failures (~90 s on a black-holed node) it runs
`iptables -I INPUT 1 -p udp --dport 51820 -j DROP` in tailguard's namespace so
WG clients see the tunnel die and fail over to their own WAN; after 2
consecutive successes (~24 s) it removes the rule. It restores the listener on
container stop (fail-open). Watch it: `docker logs -f tailguard-route-health`.
Tune `FAIL_N` / `OK_N` / `INTERVAL` / `PROBE_TIMEOUT` via `environment:` in
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

Note: this closes the WG *server* listener. LAN clients still need their own
failover (a WAN route / mwan3 / watchdog) to actually use the internet while the
tunnel is down — otherwise it's just "no traffic" until my-exit returns.

## Deploy (on the host)

### 0. Prereqs

- The host's LAN interface has a **static DHCP reservation** (`<host-lan-ip>`): the
  router's WireGuard peer uses it as its Endpoint.
- A **reusable** Tailscale auth key from the admin console (Settings > Keys).
  Untagged, owned by your user, is fine. Confirm tailnet ACLs let `tg-node`
  use `my-exit` as an exit node (default allow-all is fine).

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
# edit .env: set TS_AUTHKEY=
```

### 3. Up

```bash
docker compose up -d && docker compose logs -f
```

Expect: `wg0` up -> firewall rules -> `containerboot` -> tailscale auth OK ->
healthcheck green -> delayed script sets the exit node.

## Verify (host only, no router yet)

```bash
# tg-node is registered, my-exit is the active exit node
docker compose exec tailguard tailscale status

# read-only status page, from any other node on your tailnet:
#   curl http://<tg-node-tailscale-ip>:8088
```

Egress proof through the WG path — `./verify.sh` runs a throwaway WireGuard
client (from `config/cli-test.conf`, built from the client template below) that
pushes a download through the tunnel and reads the exit-node byte counters
before/after:

```bash
./verify.sh
```

Expect: `my-exit` peer `rx` grows by roughly the download size, i.e. the WG
client's traffic transited the exit node.

**Do not rely on the public egress IP as the signal if the host and the exit node
share an egress address** (same cloud/NAT): `curl ifconfig.co` then returns the same IP
with or without the tunnel. Use the peer counters (`tailscale status` `tx/rx` on the
exit-node line) or a Cloudflare `cdn-cgi/trace` `colo=` difference instead.

Two gotchas when running a throwaway client with `--entrypoint sh` (bypassing
`entrypoint.sh`): run `resolvconf -u` first or `wg-quick`'s `resolvconf -a`
fails with "signature mismatch"; and the client config's `DNS =` line is what
triggers that call.

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

- `AllowedIPs = 0.0.0.0/0, ::/0` — everything egresses via the exit node.
- `DNS = 1.1.1.1` over the tunnel (the client is not a Tailscale node). Use
  `100.100.100.100` instead if you want tailnet name resolution.
- `MTU = 1280` — matches `tailscale0`; avoids fragmentation in the nested
  tunnels.
- The router must keep its direct route to `<lan-subnet>` (default) so the
  handshake to `.101` isn't pulled into the tunnel.

After loading it into the router, re-run the `ifconfig.co` check from a LAN
client behind the router; confirm DNS resolves and doesn't leak.

**Rollback:** `docker compose down` here + disable the router tunnel. Nothing on
the host changes; delete `tg-node` from the Tailscale console if you want.

## Troubleshooting

| symptom | cause / fix |
|---|---|
| `cannot resolve exit node by hostname while Tailscale is starting up` | `TS_EXIT_NODE` is a hostname — use the IP (`100.x.y.z`) or `auto:any`. |
| `failed to auth tailscale ... tailscale up failed` | `TS_AUTHKEY` missing/expired/not reusable, or `TS_LOGIN_SERVER` wrong. |
| container won't bind 41642 | something else took it — pick another `TS_PORT` and update the compose port. |
| exit node not applied | check `docker compose exec tailguard tailscale status`; the exit node is re-set by a delayed script once the healthcheck passes — give it ~1 min. |
| poor throughput | nested-tunnel MTU — confirm client `MTU=1280`; check `tailscale status` for a DERP vs direct path to my-exit. |
| container replaced unexpectedly | watchtower auto-updated `:latest`; set the `watchtower.enable=false` label in `docker-compose.yml`. |

## Using headscale instead of Tailscale's own control plane

1. `.env`: `TS_LOGIN_SERVER=https://<your-headscale-url>`; swap `TS_AUTHKEY` for a
   headscale pre-auth key (`scripts/headscale-assign-host.sh` or a rotated key); set
   `EXIT_NODE_PREFIX` to the name of one of your headscale exit nodes.
2. `rm -rf state/ && docker compose up -d` to re-auth against the new control
   server. `config/wg0.conf` and the router config are unchanged.

# reality-ingress (the bridge front)

xray VLESS+Reality entry point with a Caddy "self-steal" front, for hosts that must be reachable
from inside a restrictive network (one that blocks or throttles many destinations, so a relay that
is reachable from it is needed). Destinations on the configured direct lists (some services only accept
connections from the relay's own network) leave from this host; everything else
goes to the loopback SOCKS5 port of the `tailscale-userspace-socks` role (a second userspace
tailscaled), so it exits via a tailnet exit node.

```
client --VLESS+Reality :443--> xray --direct--------------------> destination
                                |  \--via-exit-> 127.0.0.1:1055 (tailscale-userspace-socks) --> exit node
                                \--(probes / unknown SNI)--> 127.0.0.1:8443 Caddy (real Let's Encrypt cert)
```

> **HARD GATE before the first phone is enrolled:** harden the headscale ACL so the tag of the
> `tailscale-userspace-socks` node (and of this host) can reach nothing on the tailnet except the
> exit node it uses, and nothing can reach this host except the operator side on the agent port.
> The xray routing rule and the nftables backstop below are defence in depth, not a substitute: a
> phone user must not be able to reach any tailnet service even if both were wrong.

## What it installs

| Piece | Where |
|---|---|
| xray | `/usr/local/bin/xray`, geodata in `/usr/local/share/xray/`, unit `xray.service` (user `xray`) |
| Caddy | `/usr/local/bin/caddy`, unit `caddy-ingress.service` (user `caddy`), `/etc/caddy-ingress/Caddyfile`, state `/var/lib/caddy-ingress` |
| xray config | `/etc/xray/` (`root:xray`, `2750`): `config.base.json` (`0640`, holds the Reality private key) |
| nftables backstop | `/etc/ingress-guard.nft`, unit `ingress-guard.service` (table `inet ingress_guard`); package `nftables` if `nft` is missing |

Binaries are downloaded on the **control host** (hosts in restricted networks often cannot reach GitHub), verified
with `get_url` `checksum: sha256:...`, unpacked there (xray: `Xray-linux-64.zip`; Caddy:
`caddy_<ver>_linux_amd64.tar.gz`; linux/amd64 only) and copied over. Everything is staged in a fresh per-run temp dir on the control host, removed afterwards
(nothing unpacked earlier is ever reused; every download is checksum-verified first). Only
x86_64 hosts are supported (asserted from gathered facts). In `--check` mode downloads are
reported but unpacking/copying binaries and validation are skipped.

Control-host requirements: network access to GitHub, `unzip` (xray archive; present on macOS and most
Linux), and any `tar` that can read `.tar.gz` (BSD tar on macOS and GNU tar both work: the Caddy archive is
unpacked with a plain `tar -xzf`, not Ansible's `unarchive`, which rejects BSD tar).

## Variables

Required (no default; preflight fails early): `ingress_host`, `reality_private_key`,
`reality_short_id` (hex, even length, at most 16; vault), `xray_version` + `xray_sha256`,
`caddy_version` + `caddy_sha256`, `xray_geodata_files` (list of `{name, url, sha256}` with at least
`geoip.dat` and `geosite.dat`, both required), `enroll_upstream` (`host:port`, no scheme).

Optional: `ingress_listen_port` (443), `reality_sni` (= `ingress_host`), `reality_dest`
(`127.0.0.1:8443`), `socks_listen` (`127.0.0.1:1055`, must be loopback), `xray_user` (`xray`),
`xray_api_listen` (`127.0.0.1:10085`, loopback), `xray_url` / `caddy_url` (release URLs derived
from the versions), `ingress_guard_resolvers` (`["127.0.0.53"]`, IPv4 resolvers xray may query on
port 53; must include every IPv4 resolver in `/etc/resolv.conf` that lies in a range the guard blocks, such as a tailnet
resolver at `100.100.100.100` or a local stub; preflight checks this), `ingress_guard_file`, `nft_bin`, `ingress_agent_unit`
(`ingress-agent`). See `defaults/main.yml`.

## Several bridges and discovery

The bridge agent (role `ingress-agent`) serves the public parameters of this bridge at `GET /v1/info`
(`ingress_name`, `ingress_host`, `ingress_listen_port`, `reality_sni`, `reality_short_id` and the new
`reality_public_key`), so node-control can discover it and the operator approves it there. It publishes public
values only, never the private key. `reality_public_key` is the public half of the vaulted `reality_private_key`:
get it with `xray x25519 -i <private key>` and set it as a variable (this role does not run xray for it). Optional:
empty means `/v1/info` answers `503`. Use a token per bridge host. `ingress_name` is a short label such as
`ingress-1` (used only for discovery).

With several bridges, every bridge's Caddy front must serve node-control's `link_host` (a certificate for it, and
the `/e/<token>` and `/sub/<secret>` proxy to `enroll_upstream`, which is node-control's `:8081`), so the shared name
can point at any of them. Give each bridge host its own agent token (`AGENT_TOKEN_<NAME>` in node-control, one
distinct value per host).

Risk to know about: TLS for `link_host` ends on this front, so a compromised bridge that `link_host` points at can
read one-time link tokens and subscription secrets and serve phones a modified profile. Credentials are per bridge
(`uuids: derived` in node-control's `enroll.json`), which limits what such a host holds, but not this. node-control
ships `scripts/profile-canary.sh` to detect altered profiles; see "Compromised bridge" in its README.

## Who owns which config file

The role writes `/etc/xray/config.base.json`. The **bridge agent** (`apps/ingress-agent`) owns
`/etc/xray/config.json`: it renders it from the base (clients into inbound `reality-in`, routing
rules prepended, `domainStrategy` forced to `IPOnDemand`), validates it and restarts xray. The role
only seeds `config.json` from the base when it does not exist (`force: false`), so xray can start
before the agent has ever run.

- A change to `config.base.json` (Reality key, SNI, short id, ports) does **not** restart xray
  directly: it restarts the `ingress-agent` unit, if `/etc/systemd/system/ingress-agent.service`
  exists. The agent's startup restore re-renders `config.json` from the new base and restarts xray
  only if the result differs. On a first run the agent role (which runs after this one) renders it
  when the agent first starts. Check `journalctl -u ingress-agent` for `restored: <n>` (or
  `startup restore failed`, in which case xray keeps the old config). A push alone does not pick up a
  base change: an unchanged credential set is a no-op for the agent.
- Binary, geodata and unit changes do restart xray; Caddyfile/binary changes restart
  `caddy-ingress` (admin is off, so there is no reload).
- `blocked` is the FIRST outbound and the base routing ends with an explicit catch-all
  (`network: tcp,udp` -> `blocked`) behind the api rule: a client matching none of the agent's
  (prepended) rules is dropped (fail closed).

## Kernel backstop (nftables)

The agent's xray rule blocks `geoip:private` and `100.64.0.0/10` for phone users, but it acts on
the resolved address only as far as xray resolves it. The kernel backstop does not depend on name
resolution (DNS rebinding, a hostname pointing at `10.x` or `127.x`): `ingress-guard.service`
(oneshot, `After=network-pre.target`, `Before=xray.service`; `xray.service` `Requires=` it, so xray
does not run without it) loads table `inet ingress_guard` with an `output` chain. For packets sent
by uid `xray` (`meta skuid`) it:

1. accepts replies on connections xray accepted (`ct state established,related`): client
   connections and the stats API. The stats API is queried by the root-run agent, so its own
   packets are not affected; only xray's replies are, and those are established;
2. accepts `127.0.0.1` (or `::1`) TCP to the SOCKS port of `socks_listen` and the port of
   `reality_dest` (the Caddy front), as far as those are loopback;
3. accepts port 53 (UDP/TCP) to `ingress_guard_resolvers` (the systemd-resolved stub);
4. drops everything else to `0.0.0.0/8`, `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`,
   `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`, `::1`, `fc00::/7` (covers the tailnet's
   `fd7a:115c:a1e0::/48`) and `fe80::/10`.

Other users (root, the agent, caddy, both tailscaleds) are untouched. The file is validated with
`nft -c -f` before it replaces the old one; a change reloads the table atomically (`systemctl
reload ingress-guard`, never restart, which would restart xray too). `systemctl stop
ingress-guard` deletes the table. Not covered: the host's own public address, if it is configured
on an interface (on a 1:1 NAT floating IP it is not). The `direct` outbound resolves with
`domainStrategy: UseIPv4`, so IPv6 destinations are not dialled directly.

### Surviving a flush

`nft flush ruleset` deletes the table while `ingress-guard.service` stays active, so rerunning the role does not
bring it back. Two additions cover this:

- `ingress-guard-check.timer` (every `ingress_guard_check_interval`, default 30 s) runs `nft list table inet
  ingress_guard` and, if it fails, logs a warning (`journalctl -t ingress-guard-check`) and reloads the guard. The
  gap after a flush is at most one interval.
- `xray.service` carries `IPAddressDeny=` for private, CGNAT/tailnet, link-local and ULA ranges, with
  `IPAddressAllow=` for loopback and `ingress_guard_resolvers`. It is enforced by a cgroup eBPF filter that
  `nft flush ruleset` does not touch. It filters by address only, so the loopback port limits (SOCKS and the Caddy
  front only) still depend on the nftables table. It needs systemd >= 243 and cgroup BPF in the kernel; check with
  `systemctl show xray -p IPAddressDeny -p IPAddressAllow`.

## Ports

Open in the cloud security group (the role changes no host firewall): TCP **443** (Reality) and
TCP **80** (ACME HTTP-01 and renewals). Caddy's HTTPS listener (`127.0.0.1:8443`) and the xray stats
API are loopback only.

## Caddy front

`:80` answers `404` to everything except ACME HTTP-01. `<ingress_host>:8443` (bound to loopback)
is the Reality `dest`; it reverse-proxies only paths matching `^/(e|sub)/<token>$` (token 1-80 of
`A-Za-z0-9_.-`, not starting with a dot; no trailing slash, no extra segments, no encoded
slashes) to `http://<enroll_upstream>`, with `X-Forwarded-*` stripped, `Host` set to `ingress_host`
and short timeouts; anything else is an empty `404`. There is no access log. Caddy's runtime/ACME errors go to the journal (level ERROR, with the
logged request removed so enrolment tokens in paths never appear); `journalctl -u caddy-ingress`
is the place to diagnose certificate issuance. The log filter syntax is unverified until
`caddy validate` runs on the host.

### Assumptions to verify on the live host

1. **ACME/bind interplay.** The certificate comes from HTTP-01 on public `:80` while HTTPS listens
   only on `127.0.0.1:8443` (TLS-ALPN is disabled since 8443 is not reachable). This is expected to
   work because the challenge is served from the `:80` site, but it has not been run. Check:
   `journalctl -u caddy-ingress`, and `echo | openssl s_client -connect 127.0.0.1:8443 -servername <host> | openssl x509 -noout -issuer`.
   **Fallback** if Caddy refuses or never obtains a certificate: remove `bind 127.0.0.1` (the
   site then listens on `:8443` on all interfaces, still unreachable from outside unless the
   security group opens 8443) or, if that also fails, obtain the certificate with `certbot
   --standalone` / `certbot certonly --webroot` and point `tls <cert> <key>` at it.
2. `header_up -X-Forwarded-For` must really strip the header Caddy adds; check what the upstream
   receives (`X-Forwarded-*` absent).
3. Run `xray run -test` and `caddy validate` on the host (the role does this before starting).

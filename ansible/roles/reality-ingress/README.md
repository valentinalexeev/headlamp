# reality-ingress (the bridge front)

This role installs an xray VLESS+Reality entry point with a Caddy "self-steal" front. It is for hosts that must be reachable
from inside a restrictive network. Such a network blocks or throttles many destinations, so a relay that
is reachable from it is needed.

Destinations on the configured direct lists leave from this host. Some services accept connections
only from the network of the relay. Everything else
goes to the loopback SOCKS5 port of the `tailscale-userspace-socks` role (a second userspace
tailscaled). It then exits through a tailnet exit node.

```
client --VLESS+Reality :443--> xray --direct--------------------> destination
                                |  \--via-exit-> 127.0.0.1:1055 (tailscale-userspace-socks) --> exit node
                                \--(probes / unknown SNI)--> 127.0.0.1:8443 Caddy (real Let's Encrypt cert)
```

> **HARD GATE before you enrol the first phone:** harden the headscale ACL. The tag of the
> `tailscale-userspace-socks` node (and of this host) must reach nothing on the tailnet except the
> exit node that it uses. Nothing must reach this host except the operator side on the agent port.
> The xray routing rule and the nftables backstop below are defence in depth. They do not replace the ACL.
> A phone user must not be able to reach any tailnet service, even if both were wrong.

## Supported systems

The role uses `apt` and systemd. The OS matrix (`make test-ansible-os`, #170) tests it on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13. All six roles pass check mode and a real apply on these three systems, on amd64 and arm64 (#178).

For a bridge VM, use Ubuntu 26.04 Minimal. It is the preferred image. You can also use Debian 13.

## What it installs

| Piece | Where |
|---|---|
| xray | `/usr/local/bin/xray`, geodata in `/usr/local/share/xray/`, unit `xray.service` (user `xray`) |
| Caddy | `/usr/local/bin/caddy`, unit `caddy-ingress.service` (user `caddy`), `/etc/caddy-ingress/Caddyfile`, state `/var/lib/caddy-ingress` |
| xray config | `/etc/xray/` (`root:xray`, `2750`): `config.base.json` (`0640`, holds the Reality private key) |
| nftables backstop | `/etc/ingress-guard.nft`, unit `ingress-guard.service` (table `inet ingress_guard`); package `nftables` if `nft` is missing |

The role gets the binaries on the **control host**, because hosts in restricted networks often cannot reach a
registry or GitHub. The bridge VM does not need docker. The role supports `x86_64` (amd64) and `aarch64` (arm64)
hosts. It uses the image manifest and the sha256 pin of the host architecture.

By default, the files come from the official images, pinned by digest (#182):

| File | Image | Path in the image |
|---|---|---|
| `xray` | `ghcr.io/xtls/xray-core` (`xray_image`) | `/usr/local/bin/xray` |
| `geoip.dat`, `geosite.dat` | the same image | `/usr/local/share/xray/` |
| `caddy` | `docker.io/library/caddy` (`caddy_image`) | `/usr/bin/caddy` |

The script `files/oci-extract.py` pulls only the necessary layers from the registry. It uses the Python of
`ansible-playbook` and its standard library only. It does not need docker, `skopeo` or `crane`. The script checks the
digest of each manifest and each layer. Then the role checks the sha256 of each file against the pin
(`xray_image_sha256`, `caddy_image_sha256`, the `sha256` of each geodata item).

`xray_image_sha256` and
`caddy_image_sha256` are maps with the keys `amd64` and `arm64`. When you change a digest, change all values.

We use this method because the GitHub release CDN can fail for hours (HTTP 504). The registries of the images are a
different service. The digest and the sha256 pin the exact content.

To use a mirror, set `xray_url` with `xray_sha256`, or `caddy_url` with `caddy_sha256`. The role then downloads that
release archive with `get_url` and checks its sha256. xray: the `Xray-linux-64.zip` archive. Caddy: the
`caddy_<ver>_linux_<arch>.tar.gz` archive. The archive must agree with the host architecture. A geodata item with a `url` comes from that URL. A geodata item without a
`url` comes from `xray_image`.

The role keeps each file in a cache on the control host (#198): `reality_ingress_cache_dir`, default
`~/.cache/headlamp/reality-ingress`. The path of a file is `<cache>/<sha256>/<name>`. The role gets a file only when
it is not in the cache.

On each run, the role calculates the sha256 of each cached file again. If the sha256 is not
correct, the role gets the file again. The role verifies the checksum of every file before it copies the file. You
can delete the cache. The next run fills it again.

The role asserts the architecture from gathered facts. In `--check` mode, the role fills the cache on the control
host. It shows only the changes on the host. A run with nothing to do shows `changed=0`, in `--check` mode and in a
real run. In `--check` mode, the role does not do the validation.

Control-host requirements:

- Network access to `ghcr.io` and `registry-1.docker.io` (the default), or to the URLs that you set.
- A CA store for the Python of `ansible-playbook`. The script uses `certifi` when it is installed.
- `unzip` for the xray archive, only with `xray_url` (present on macOS and most Linux).
- Any `tar` that can read `.tar.gz`, only with `caddy_url`. BSD tar on macOS and GNU tar both work. The role unpacks
  the Caddy archive with a plain `tar -xzf`, not with `unarchive` of Ansible, which rejects BSD tar.

To update a pin:

1. Get the digest of the new image: `docker buildx imagetools inspect ghcr.io/xtls/xray-core:<version>`.
2. Get the files: `python3 files/oci-extract.py <image>@<digest> <dir> /usr/local/bin/xray`.
3. Get the sha256 of each file: `shasum -a 256 <dir>/*`.
4. Set the digest and the sha256 values together.

## Variables

Required (no default; preflight fails early):

- `ingress_host`
- `reality_private_key`
- `reality_short_id` (hex, even length, at most 16; vault)
- `enroll_upstream` (`host:port`, no scheme)
- `enroll_self_service` (default `false`): also proxy the self-enrolment pages (see "Caddy front")
- `hy2_obfs_password` when `hy2_enabled` is true (the default): 16 to 128 characters of `[A-Za-z0-9_-]`, from a vault.
  Make it with `openssl rand -hex 16`.

Optional:

- `ingress_listen_port` (443)
- `reality_sni` (= `ingress_host`)
- `reality_dest` (`127.0.0.1:8443`)
- `socks_listen` (`127.0.0.1:1055`, must be loopback)
- `xray_user` (`xray`)
- `xray_api_listen` (`127.0.0.1:10085`, loopback)
- `xray_image`, `xray_image_sha256`, `caddy_image`, `caddy_image_sha256`: the image pins (see above).
  `xray_image_path`, `xray_image_geodata_dir` and `caddy_image_path` give the paths in the images.
- `xray_url` with `xray_sha256`, and `caddy_url` with `caddy_sha256` (empty): the archive override for a mirror.
  `xray_version` and `caddy_version` are not used by the role. You can use them to make the URL.
- `xray_geodata_files`: a list of `{name, sha256}` and an optional `url`. It must have `geoip.dat` and `geosite.dat`.
  The default takes both from `xray_image`.
- `ingress_guard_resolvers` (`["127.0.0.53"]`): IPv4 resolvers that xray can query on port 53.
  The list must include every IPv4 resolver in `/etc/resolv.conf` that lies in a range that the guard blocks.
  Examples are a tailnet resolver at `100.100.100.100` or a local stub. Preflight checks this.
- `ingress_guard_file`
- `nft_bin`
- `ingress_agent_unit` (`ingress-agent`)
- `hy2_enabled` (`true`), `hy2_listen_port` (`443`), `hy2_tls_dir` (`/etc/xray/tls`), `hy2_cert_sync_interval` (`6h`)

See `defaults/main.yml`.

## Hysteria 2

The role adds a Hysteria 2 inbound (`hy2-in`) on UDP, next to the Reality inbound on TCP (#110). xray serves both.
The ingress agent adds every credential to both inbounds. Thus the modes, the block of private and tailnet ranges,
and the statistics are the same for the two protocols. A client profile has both outbounds for each mode. The
client uses the first one that works, so it uses VLESS Reality when a network blocks UDP.

- **Certificate.** Hysteria 2 uses the HTTP-01 certificate that Caddy gets for `ingress_host`. The timer
  `hy2-cert-sync` copies it to `hy2_tls_dir` and restarts xray when the certificate changes.
- **Obfuscation.** Salamander is on, with `hy2_obfs_password`. Without the password the bridge does not answer.
  Thus a scan of the UDP port sees nothing. node-control gets the same value in `HY2_OBFS_<NAME>`, for example
  `HY2_OBFS_INGRESS_1` for the bridge `ingress-1`. It puts the value only into the client profiles.
- **Discovery.** The agent publishes `hy2_port` in `/v1/info`. A bridge that adds Hysteria 2 publishes a new
  fingerprint, so an operator must approve it again.
- To turn it off, set `hy2_enabled: false`. The bridge then serves VLESS Reality only, as before.

`make test-hysteria-e2e` runs the full flow in Docker: tailnet, exit node, bridge, enrolment and real clients.

## Several bridges and discovery

The bridge agent (role `ingress-agent`) serves the public parameters of this bridge at `GET /v1/info`
(`ingress_name`, `ingress_host`, `ingress_listen_port`, `reality_sni`, `reality_short_id` and the new
`reality_public_key`). Node-control can then discover the bridge, and the operator approves it there. The agent publishes public
values only, never the private key.

`reality_public_key` is the public half of the vaulted `reality_private_key`.
Get it with `xray x25519 -i <private key>` and set it as a variable. This role does not run xray for it. The variable is optional.
If it is empty, `/v1/info` answers `503`.

Use a token for each bridge host. `ingress_name` is a short label such as
`ingress-1` (used only for discovery).

With several bridges, the Caddy front of every bridge must serve the `link_host` of node-control. This needs a certificate for it.
It also needs the `/e/<token>`, `/sub/<secret>` and `/sub/<secret>/<format>` proxy to `enroll_upstream`, which is `:8081` of node-control. The shared name
can then point at any of the bridges. Give each bridge host its own agent token (`AGENT_TOKEN_<NAME>` in node-control, one
distinct value for each host).

**WARNING:** Know this risk. TLS for `link_host` ends on this front. A compromised bridge that `link_host` points at can
read one-time link tokens and subscription secrets. It can also serve phones a modified profile.

Credentials are different for each bridge (`uuids: derived` in `enroll.json` of node-control). This limits what such a host holds, but it does not limit this risk.
node-control ships `scripts/profile-canary.sh` to detect altered profiles. See "Compromised bridge" in its README.

## Policy needed by a bridge

The headscale policy must let the bridge (`tag:ingress`) use its exit node for the internet (`autogroup:internet`),
and let node-control reach the bridge agent. Without the `autogroup:internet` rule, clients connect but get no
reply (#259). See "Policy needed by a bridge" in the `tailscale-userspace-socks` README for the rules. The example
policy `examples/compose/control/config/acl.json` has them.

## Who owns which config file

The role writes `/etc/xray/config.base.json`. The **bridge agent** (`apps/ingress-agent`) owns
`/etc/xray/config.json`. The agent renders it from the base (clients into inbound `reality-in`, routing
rules prepended, `domainStrategy` set to `IPOnDemand`), validates it and restarts xray. The role
seeds `config.json` from the base only when it does not exist (`force: false`). Then xray can start
before the agent has ever run.

- A change to `config.base.json` (Reality key, SNI, short id, ports) does **not** restart xray
  directly. It restarts the `ingress-agent` unit, if `/etc/systemd/system/ingress-agent.service`
  exists. The startup restore of the agent renders `config.json` again from the new base. It restarts xray
  only if the result differs. On a first run, the agent role (which runs after this one) renders it
  when the agent first starts. Check `journalctl -u ingress-agent` for `restored: <n>` (or
  `startup restore failed`, in which case xray keeps the old config). A push alone does not pick up a
  base change, because an unchanged credential set is a no-op for the agent.
- Binary, geodata and unit changes do restart xray. Caddyfile and binary changes restart
  `caddy-ingress` (admin is off, so there is no reload).
- `blocked` is the FIRST outbound. The base routing ends with an explicit catch-all
  (`network: tcp,udp` -> `blocked`) behind the api rule. A client that matches none of the (prepended) rules of the agent
  is dropped (fail closed).

## Tailnet access (`via-tailnet`)

The base config has the outbound `via-tailnet`, a second name for the SOCKS port of `socks_listen` (#132). The agent uses it only for credentials with a tailnet grant, and only for tailnet addresses. node-control gives the grant per bridge (`"tailnet_access": "all"`). The headscale ACL for the tag of `socks_authkey` decides which hosts answer. The kernel backstop does not change, because xray dials only the loopback SOCKS port.

For tailnet grants (#139), set `socks_grant_groups` to the same list as in the `tailscale-userspace-socks` role. The base config then has one outbound `via-tailnet-<group id>` for each entry. The first entry uses `127.0.0.1:<socks_grant_port_base>` (default `1056`), the next entry uses the next port. Only `id` of each entry is read here.

## Kernel backstop (nftables)

The xray rule of the agent blocks `geoip:private` and `100.64.0.0/10` for phone users. It acts on
the resolved address only as far as xray resolves it. The kernel backstop does not depend on name
resolution (DNS rebinding, a hostname that points to `10.x` or `127.x`).

`ingress-guard.service` is oneshot, with `After=network-pre.target` and `Before=xray.service`.
`xray.service` has `Requires=` for it, so xray does not run without it. The unit loads table `inet ingress_guard` with an `output` chain.
For packets sent by uid `xray` (`meta skuid`), the table does this:

1. It accepts replies on connections that xray accepted (`ct state established,related`). These are client
   connections and the stats API. The root-run agent queries the stats API, so its own
   packets are not affected. Only replies of xray are affected, and those are established.
2. It accepts TCP on `127.0.0.1` (or `::1`) to two ports. They are the SOCKS port of `socks_listen`
   and the port of `reality_dest` (the Caddy front). This applies as far as those are loopback.
3. It accepts port 53 (UDP/TCP) to `ingress_guard_resolvers` (the systemd-resolved stub).
4. It drops all other traffic to these ranges: `0.0.0.0/8`, `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`,
   `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`, `::1`, `fc00::/7` and `fe80::/10`.
   The range `fc00::/7` covers `fd7a:115c:a1e0::/48` of the tailnet.

Other users (root, the agent, caddy, both tailscaleds) are not affected. The role validates the file with
`nft -c -f` before it replaces the old one. A change reloads the table atomically (`systemctl
reload ingress-guard`). Never restart it, because that restarts xray too. `systemctl stop
ingress-guard` deletes the table.

The table does not cover the public address of the host itself, if it is configured
on an interface. (On a 1:1 NAT floating IP it is not.) The `direct` outbound resolves with
`domainStrategy: UseIPv4`, so xray does not dial IPv6 destinations directly.

### Surviving a flush

`nft flush ruleset` deletes the table while `ingress-guard.service` stays active, so a rerun of the role does not
bring it back. Two additions cover this:

- `ingress-guard-check.timer` (every `ingress_guard_check_interval`, default 30 s) runs `nft list table inet
  ingress_guard`. If this fails, the timer logs a warning (`journalctl -t ingress-guard-check`) and reloads the guard. The
  gap after a flush is at most one interval.
- `xray.service` carries `IPAddressDeny=` for private, CGNAT/tailnet, link-local and ULA ranges. It has
  `IPAddressAllow=` for loopback and `ingress_guard_resolvers`. A cgroup eBPF filter enforces it, and
  `nft flush ruleset` does not touch that filter. It filters by address only, so the loopback port limits (SOCKS and the Caddy
  front only) still depend on the nftables table. It needs systemd >= 243 and cgroup BPF in the kernel. Check with
  `systemctl show xray -p IPAddressDeny -p IPAddressAllow`.

## Ports

Open these ports in the cloud security group. The role changes no host firewall.

- TCP **443**: Reality.
- UDP **443**: Hysteria 2, unless `hy2_enabled` is false.
- TCP **80**: ACME HTTP-01 and renewals. The HTTPS listener of Caddy (`127.0.0.1:8443`) and the xray stats
API are loopback only.

## Caddy front

`:80` answers `404` to everything except ACME HTTP-01.

`<ingress_host>:8443` (bound to loopback) is the Reality `dest`. The public paths come from `files/enroll-paths.caddy`
(#260), which the role copies next to the Caddyfile. `examples/compose/control` uses the same file.

The front reverse-proxies only these paths to
`http://<enroll_upstream>`: `/e/<token>`, `/sub/<token>` and `/sub/<token>/<format>`. The format is `sing-box`, `clash`,
`stash` or `uri` (#241). The token is 1-80 characters of
`A-Za-z0-9_.-` and does not start with a dot. No trailing slash, no other segments and no encoded
slashes are allowed.

The proxy strips `X-Forwarded-*`, sets `Host` to `ingress_host` and uses short timeouts. Anything else gets an empty
`404`.

With `enroll_self_service: true`, the front also proxies the pages of self-enrolment with OIDC (#102). The sign-in
always runs on `link_host` (#270). On a front with another name, `/me`, `/oidc/login` and `/i/<token>` send the
browser to the same page on `link_host`. That is where the redirect URL of the identity provider points. These are `/me`, `/me/devices`, `/me/devices/<id>`, `/me/devices/<id>/revoke`, `/me/logout`, `/me/icons/<app>`, `/i/<token>`, `/oidc/login` and `/oidc/callback`, and nothing below them. The default is `false`.

There is no access log. Runtime and ACME errors of Caddy go to the journal (level ERROR). The logged request is removed,
so enrolment tokens in paths never appear. `journalctl -u caddy-ingress`
is the place to diagnose certificate issuance. The log filter syntax is unverified until
`caddy validate` runs on the host.

### Assumptions to verify on the live host

1. **ACME/bind interplay.** The certificate comes from HTTP-01 on public `:80`, while HTTPS listens
   only on `127.0.0.1:8443`. TLS-ALPN is disabled, because 8443 is not reachable. This is expected to
   work, because the `:80` site serves the challenge. It has not been run. Check:
   `journalctl -u caddy-ingress`, and `echo | openssl s_client -connect 127.0.0.1:8443 -servername <host> | openssl x509 -noout -issuer`.

   **Fallback** if Caddy refuses or never obtains a certificate:

   1. Remove `bind 127.0.0.1`. The site then listens on `:8443` on all interfaces. It is still unreachable from outside, unless the
      security group opens 8443.
   2. If that also fails, obtain the certificate with `certbot
      --standalone` or `certbot certonly --webroot`. Point `tls <cert> <key>` at it.
2. `header_up -X-Forwarded-For` must really strip the header that Caddy adds. Check what the upstream
   receives (`X-Forwarded-*` absent).
3. Run `xray run -test` and `caddy validate` on the host (the role does this before it starts the services).

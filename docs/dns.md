# DNS in headlamp

This manual tells how each part of headlamp uses DNS. It tells which resolver each part asks, which path the query
uses, and which settings control it. The last sections show which answer each type of client gets, and the limits.

Each section gives the setting names. The component manuals give the full procedures.

## Summary

| Part | It resolves | Resolver | Path of the query | Settings |
|---|---|---|---|---|
| Tailnet device | All names | tailscale on the device (`100.100.100.100`) | Local; other names go to the global nameservers of headscale | headscale `dns:` |
| Device that uses an exit node | Public names | The exit node, or the global nameservers | Over the tailnet to the exit node | `autogroup:internet` in the policy |
| Bridge (xray) | Destinations of clients, for the routing rules | The `dns` section that the agent writes | Through `via-exit`; direct-list names use the system resolver | `INGRESS_EXIT_DNS`, `INGRESS_DIRECT_GEOSITE`, `tailnet_dns` |
| Bridge (egress self-test) | The host of the test URL | The exit node | SOCKS `CONNECT` by name | `INGRESS_EGRESS_SOCKS`, `INGRESS_EGRESS_URL` |
| Exit node container | Names in `direct-hosts.txt` | The resolver of the container | `dig` | `DIRECT_HOSTS_FILE` |
| VM exit node | `route_hosts`, `route_hosts_direct`, ASN lookups | The resolver of the host | `dig` | `route_*` role variables |
| node-control | Queries from tailnet devices and bridges | Its own DNS server on its tailnet address | Port 53, UDP and TCP | `SERVE_*`, `TAILNET_DNS_*` |
| node-control (funnel) | Writes the public wildcard record | Route 53 or Cloudflare API | HTTPS | `FUNNEL_DNS_*` |
| funnel-ingress | Public names of services | Public DNS | Internet to the TLS edge | `FUNNEL_PUBLIC_DOMAIN` |
| funnel-sidecar | Upstream services | Cluster DNS of the pod | Kubernetes | `FUNNEL_SERVICES` |
| tailnet connector | Nothing for clients | None | None | `TS_ACCEPT_DNS` |
| tailguard client | All names | `DNS =` of the WireGuard client | Through the tunnel and the exit node | `DNS =`, `TG_NAMESERVERS` |

## headscale

headscale gives the DNS configuration to each node in the netmap. The settings are in the `dns:` block of
`headscale.yaml`. headscale reads this file only when it starts. Thus a change to the file needs a restart.

| Setting | Value in the examples | What it does |
|---|---|---|
| `magic_dns` | `true` | Each node gets the name `<given name>.<base_domain>` |
| `base_domain` | `tailnet.example.com` | The MagicDNS domain |
| `override_local_dns` | `true` | Devices send all queries to tailscale, also queries for public names |
| `nameservers.global` | `1.1.1.1`, `1.0.0.1` | Resolvers for names that tailscale does not answer |
| `nameservers.split` | not set | Sends one zone, or one name, to a given resolver |
| `extra_records` | not set | Static A and AAAA records for all nodes |
| `extra_records_path` | not set | A JSON file of records. headscale reads it again when it changes |

The examples are `apps/headscale/headscale.yaml` and `examples/compose/control/config/headscale.yaml`.

Obey these rules:

- `base_domain` must be different from the domain of `server_url`.
- Do not put a split zone, a serve zone or a funnel domain under `base_domain`. Devices answer all names under
  `base_domain` themselves. They never send these names to a split resolver.
- headscale has no API for DNS. The API and the policy cannot add records or nameservers.

## Tailnet devices

A tailnet device sends its queries to tailscale (`100.100.100.100`). tailscale answers from the netmap in this order:

1. MagicDNS names of the nodes.
2. The extra records of headscale. A name can have more than one record. tailscale then gives the first address of
   each type (A or AAAA).
3. Split zones. tailscale sends the query to the resolver of the zone, for example node-control for the serve zone.
4. Other names. tailscale sends the query to the global nameservers.

tailscale 1.104 and later adds a DNS route for each extra record. Thus the device sends these names to tailscale also
without `override_local_dns`. The operating system must support split DNS.

When a device uses an exit node, tailscale can send its public queries to the exit node. Then the policy must give
`autogroup:internet` to the device. If not, the lookups fail with `403 Forbidden`.

### Split horizon

A name can have a public address and a tailnet address. For example, the public DNS gives `proxy.example.com` the
address of a public proxy. An extra record gives the same name a tailnet address. A tailnet device then gets the
tailnet address. Other computers get the public address.

The split applies to all tailnet devices. headscale sends the same extra records to all nodes. Thus two tailnet users
cannot get different answers from extra records. The policy decides who can connect.

For answers per user or tag, use the rules of the tailnet DNS module (#285, see "Tailnet DNS module"). Then
node-control answers the name. A device needs a `nameservers.split` line for it.

A device gets the public address in these conditions:

- tailscale is not connected.
- The setting "Use Tailscale DNS" in the app is off.

## Bridges

A bridge has two DNS paths: the resolver of xray, and the exit node. The ingress-agent writes the `dns` section of the
xray configuration. It replaces the `dns` section of the base file. The tag of the section is `dns-internal`.

### The resolver of xray

`routing.domainStrategy` is `IPOnDemand`. xray resolves a destination name before it applies the first rule that has
IP addresses. Thus a name that points to a private address cannot bypass the block rule. The first routing rule sends
the queries of the `dns-internal` tag to `via-exit`.

xray uses the servers in this order. `queryStrategy` is `UseIPv4`.

| Order | Server | Names | Condition |
|---|---|---|---|
| 1 | node-control, `tcp://<tailnet ipv4>:53` | The tailnet names from node-control | The bridge has tailnet access, and the tailnet DNS module is on |
| 2 | `localhost` (the system resolver) | The `geosite:` lists of `INGRESS_DIRECT_GEOSITE` | The list is not empty |
| 3 | `INGRESS_EXIT_DNS` | All other names | Always. The default is `https://1.1.1.1/dns-query` and `https://1.0.0.1/dns-query` |

The first two servers have `skipFallback`. Thus they get only their own names. The node-control server also has
`finalQuery` and `expectedIPs` with the tailnet ranges. A failed query for a tailnet name does not go to another
server. An answer that is not a tailnet address is dropped.

Each `INGRESS_EXIT_DNS` entry must be `https://<ipv4>/<path>` or `tcp://<ipv4>[:port]`. A host name is not accepted,
because its own lookup would use the local network. The role variable is `ingress_exit_dns`.

**CAUTION:** Clients of mode `direct` also get their addresses from the exit DNS servers. When the exit node is not
available, their lookups fail.

### Tailnet names on a bridge

node-control sends the tailnet resolver to each bridge with `tailnet_access` `all` or `grants` (#286). The resolver
is the address of node-control and the list of the tailnet names. The agent keeps it in `tailnet-dns.json`, next to
`credentials.json`.

The query goes through `via-exit` to the tailscaled of the exit node on the bridge. That tailscaled sends the query
over the tailnet to node-control. The answer is a tailnet address. The tailnet rule of a client with a grant then
sends the connection to the tailnet.

The xray resolver is the same for all clients of a bridge. A client without a grant also gets the tailnet address of
a name in the list. The block rule then stops the connection.

The policy must let the bridge tag reach node-control on TCP port 53. For example:
`{"action": "accept", "src": ["tag:ingress"], "dst": ["tag:node-control:53"]}`.

**WARNING:** Without this policy rule, xray cannot resolve the tailnet names. The lookups of these names fail. They
never go to the public resolvers.

### The SOCKS tailscaled on the bridge

The `tailscale-userspace-socks` role starts tailscaled with `--accept-dns=false`. The tailscaleds of the grant groups
use the same flag. Thus these tailscaleds do not change the resolver of the host.

The egress self-test of the agent sends a SOCKS `CONNECT` with the host name of `INGRESS_EGRESS_URL`. The exit node
resolves the name.

### The kernel guard on the bridge

The `ingress-guard` nftables rules let the `xray` user send DNS only to the resolvers in `ingress_guard_resolvers`.
The default is `127.0.0.53`. The rules drop the other private ranges, the tailnet range and loopback for that user. A
DNS answer thus cannot send xray to a private address. The `xray.service` unit has the same list in
`IPAddressAllow`.

Add `100.100.100.100` to `ingress_guard_resolvers` if tailscale controls `/etc/resolv.conf`. The preflight reads
`/etc/resolv.conf`. It stops the run when a resolver is in a blocked range and is not in the list.

### Certificates and names of the bridge

- `ingress_host` must resolve to the bridge. Caddy gets its certificate with the HTTP-01 challenge on port 80. Bridges
  use no DNS-01 challenge.
- The Hysteria 2 inbound uses the same certificate. `hy2-cert-sync` copies it every 6 hours (`hy2_cert_sync_interval`).
- `INGRESS_HOST` and `INGRESS_SNI` must be DNS names.
- `link_host` must resolve to a bridge, or to the edge of the control host. Set up its DNS record before the first
  enrolment.

### Phone profiles

The profiles that node-control makes set the DNS of the phone app:

- sing-box sends the DNS of the tunnel to `tls://1.1.1.1`, through the selected bridge. It resolves the bridge host
  names with the local resolver.
- Clash sends DNS to `tls://1.1.1.1#select`. It resolves the bridge host names with the system resolver.

## Exit nodes and route provisioning

An exit node resolves host names to make `/32` routes. Thus some services see the address of the node.

The exit node container resolves each name in `direct-hosts.txt` (`DIRECT_HOSTS_FILE`) with `dig +short A`. It keeps
IPv4 addresses only. It advertises the `/32` routes, at start and every 3 hours. When no name resolves, it does not
change the routes.

On a VM node, the `route-provisioner` role does the same with these variables:

| Variable | Default | What it does |
|---|---|---|
| `route_hosts` | `[]` | Host names to resolve. Their routes are published |
| `route_hosts_direct` | `[]` | Host names to resolve to `/32` routes, with no ASN expansion |
| `route_asns` | `[]` | ASNs to publish. The role finds the ASN of an address with the DNS TXT name `<reversed ip>.origin.asn.cymru.com` |
| `route_verify_reachability` | `true` | Publishes an address only after a TCP probe |
| `route_probe_ports`, `route_probe_timeout_seconds`, `route_probe_attempts` | `[443]`, `5`, `2` | The probe |
| `route_cron_interval_hours` | `3` | How often the role resolves the names again |

A name that does not resolve is not published, and the role logs a warning. The role installs `dnsutils`.

The `tailscale` role, the exit node container and the cloud-init of the VM modules do not set `--accept-dns`. Thus
the default of tailscale applies.

## node-control

### The DNS server

node-control has one DNS server on port 53 (UDP and TCP) of its tailnet IPv4 address. The serve module and the
tailnet DNS module use the same server. A query goes to these sources, in this order:

1. The records of the tailnet DNS module.
2. The headscale extra records. node-control reads them from its own netmap.
3. The serve zone.
4. The MagicDNS names of the nodes, from the headscale API.

The first source that has the name, or the zone of the name, gives the answer. Other names get `REFUSED`. The server
never sends a query to another resolver. An unknown name in a known zone gets `NXDOMAIN`.

The server answers only A and AAAA queries. The TTL is 30 seconds. A UDP answer larger than 512 bytes is truncated,
and the client then uses TCP.

### Serve

The serve module answers the names of the serve zone (`SERVE_ZONE`). A name gives the addresses of the online proxies
of its service. The answers follow the proxies every `SERVE_DNS_REFRESH` (default 15 seconds). A service can be a
wildcard, for example `*.apps.svc.example.com`.

Tailnet devices ask node-control for the serve zone through split DNS. Add the zone to `headscale.yaml` one time:

```yaml
dns:
  nameservers:
    split:
      svc.example.com:
        - 100.64.0.1
```

The Serve page shows this line with the address of node-control. node-control keeps its address while its state
volume stays. Serve does not start when the zone is under `base_domain`, or contains it.

### Tailnet DNS module

The tailnet DNS module (#286) makes node-control the resolver of the tailnet names for bridges. It also keeps records
that an operator adds with the API. Set `TAILNET_DNS_DB` (SQLite), or `TAILNET_DNS_REDIS=1` with `REDIS_URL`, to turn
it on.

A record has one type (#285):

- `A`: one target. `addresses` is one to 8 fixed tailnet addresses. `node` is the given name of one headscale node.
  `tag` is a tag: the answer gives the addresses of the online nodes with the tag. The record answers A and AAAA
  queries.
- `CNAME`: a target name. node-control adds the records of the target when it has them.
- `TXT`: one to 8 strings.
- `SRV`: a target name, a port, a priority and a weight.

An address in an answer is always a tailnet address (`100.64.0.0/10` and `fd7a:115c:a1e0::/48`). A record name must
not be under `base_domain` or in the serve zone.

A record can have rules. A rule names tailnet logins (`users`) and tags (`tags`). It has its own data.
node-control finds the client with WhoIs on the source address of the query. The first rule that matches gives the
answer. A client that no rule matches gets the data of the record. A record without rules answers all clients the
same.

A zone makes node-control answer all names under it. A name in the zone without a record does not exist. One split
line sends the full zone to node-control.

The API is `GET /dns/state`, `POST /dns/records`, `PUT /dns/records/{name}`, `DELETE /dns/records/{name}`,
`POST /dns/zones` and `DELETE /dns/zones/{name}`. The console has a **DNS** page (permission `dns.view`, changes with
`dns.admin`). See the node-control README for the procedure and the permissions.

headscale has no API for DNS. node-control gives its records to tailnet devices in one of two ways:

| Way | Where | What the device does |
|---|---|---|
| The records file (`TAILNET_DNS_EXTRA_RECORDS_FILE`) | node-control and headscale share a directory, for example the compose stack | Resolves the names itself, from the netmap |
| A `nameservers.split` line for each name or zone | node-control and headscale are on different hosts, for example Fly.io. Also each zone, each `CNAME`, `TXT` or `SRV` record, and each record with rules | Sends the names to node-control |

With the records file, node-control writes its records into the file of `dns.extra_records_path`. When the file
changes, headscale reads it again, with no restart. The file changes only when the records change. Each write
replaces the file by rename. Thus mount the directory, not the file.

The file contains only the `A` records of node-control without rules. headscale cannot keep other types. It sends the same records to all devices.

**CAUTION:** headscale does not start when the file of `dns.extra_records_path` does not exist. Create it with `[]`
first.

A `tag` target with more than one online node gives all addresses to bridges. A tailnet device gets one of them.
When that node goes offline, it leaves the file at the next refresh.

The split line makes the name depend on node-control. When node-control stops, devices cannot resolve the name.

### Public records for funnel

The funnel module can keep one public record: `*.<FUNNEL_PUBLIC_DOMAIN>`. It points to `FUNNEL_INGRESS_HOSTNAME`.

| Setting | What it does |
|---|---|
| `FUNNEL_DNS_PROVIDER` | `route53` or `cloudflare`. If not set, the Funnel page shows a checklist, and you add the record yourself |
| `FUNNEL_DNS_ZONE` | The ID of the zone (required with a provider) |
| `FUNNEL_DNS_CLOUDFLARE_TOKEN` | Cloudflare only: a token that can edit DNS in that zone |

An address target gives an A or AAAA record. A host name target gives a CNAME record. The TTL is 300 seconds.
node-control checks the record every 10 minutes. It does not use a provider for a `.fly.dev` target.

`FUNNEL_PUBLIC_DOMAIN` must not be under `base_domain`.

### Other DNS settings of node-control

- `ENROLL_ALLOWED_HOSTS` is the list of accepted `Host` headers. It stops DNS rebinding attacks on the public listener.
- node-control finds the bridge agents through the headscale API, not through DNS.

## funnel-ingress

A public service has the name `https://<service>.<namespace>.<public domain>/`. The public domain comes from
node-control.

- On the compose stack, Caddy gets certificates on demand. It asks funnel-ingress if a name has a route.
- On Fly.io, add the certificate `*.<namespace>.<domain>` with `fly certs add`. Add the `_acme-challenge` CNAME
  record that Fly.io shows.

A route that a publisher registers keeps the tailnet address of the publisher. Set `FUNNEL_CONFIG_URL` to the tailnet
address of node-control, because the container does not use tailnet DNS.

The funnel-sidecar dials its upstream services (`FUNNEL_SERVICES`) with the cluster DNS of its pod. In serve mode, the
name of the service comes from node-control with the certificate.

## Kubernetes connector

The tailnet connector starts tailscale with `--accept-dns=false` (`TS_ACCEPT_DNS`). The connector does not give DNS
for `cluster.local` names to the tailnet. Use the ClusterIP addresses, or give names with the serve proxies of the
chart.

Bridge clients cannot reach the routes of the connector, because the agent blocks private ranges. Subnet routes for
bridge clients are a different issue.

## tailguard

The WireGuard client uses the resolver in its `DNS =` line. The example is `1.1.1.1`. The query goes through the
tunnel and the exit node. To resolve tailnet names, use `100.100.100.100`.

- `TG_NAMESERVERS` sets the resolvers in the tailguard container. If it is not set, the image uses `1.1.1.1`.
- `TS_EXIT_NODE` must be an address or `auto:any`. A host name is not accepted.
- Run `resolvconf -u` before `wg-quick`, because the `DNS =` line calls `resolvconf -a`.

## Control host

On the compose stack, the Caddy edge gets the certificates. The DNS records are yours to make.

| Name | Record | Certificate |
|---|---|---|
| `HEADSCALE_DOMAIN` | To the control host | HTTP-01 |
| `ENROLL_DOMAIN` | To the control host | HTTP-01 |
| `*.<FUNNEL_DOMAIN>` | To the control host | On demand (HTTP-01), or DNS-01 |

For DNS-01, set `FUNNEL_DNS` to `route53`, `cloudflare`, `googleclouddns` or `rfc2136`. Add the plugins to
`EDGE_DNS_PLUGINS`. With DNS-01, add one `*.<namespace>.<FUNNEL_DOMAIN>` site for each namespace. A name that ends in
`.invalid` gets a certificate from the internal CA of Caddy.

The `control-host/aws` Terraform module makes no DNS records. With `route53_zone_id`, it lets the host change records
in that zone for the DNS-01 challenge. Its outputs give the address for your record (`public_address`) and the zone of
the load balancer for an alias record (`load_balancer_zone_id`).

## Who gets which answer

The table uses the default settings of each component.

| Name | Tailnet device | Bridge client with a grant | Bridge client without a grant, on a bridge with tailnet access | Client of a bridge without tailnet access | Internet |
|---|---|---|---|---|---|
| MagicDNS name | Tailnet address | Tailnet address | Tailnet address, then blocked | No tailnet answer | No tailnet answer |
| Extra record with a public twin | Tailnet address | Tailnet address | Tailnet address, then blocked | Public address | Public address |
| Record of the tailnet DNS module | Tailnet address, with the records file or a split line | Tailnet address | Tailnet address, then blocked | Public address, if one exists | Public address, if one exists |
| Record with rules | The answer of the first rule that matches the device, with a split line | The answer for the exit node of the bridge | The same, then blocked | Public address, if one exists | Public address, if one exists |
| Serve name | Proxy addresses, with the split line | Proxy addresses | Proxy addresses, then blocked | No tailnet answer | No tailnet answer |
| Funnel name | Public address of the edge | Public address of the edge | Public address of the edge | Public address of the edge | Public address of the edge |
| Public name | Global nameservers, or the exit node | Exit DNS, or the system resolver for direct lists | The same | The same | Public DNS |

The tailnet DNS module must be on for the bridge columns with tailnet answers.

## Limits

- headscale has no API for DNS. Without a directory that node-control and headscale share, each record needs a
  split line in `headscale.yaml` and a restart of headscale.
- The resolver of a tailnet device gives one address of each type for a name. It does not spread the load over the
  nodes of a tag.
- An extra record is for one exact name. headscale and tailscale do not accept a wildcard.
- The xray resolver of a bridge is the same for all its clients. Thus the bridge cannot give a different answer to a
  client with no grant. The rules of a record do not see the bridge client.
- A record with rules, and a `CNAME`, `TXT` or `SRV` record, depend on node-control. When node-control stops, these
  names do not resolve.
- Bridge clients cannot reach the subnet routes of the tailnet, for example a Kubernetes connector.

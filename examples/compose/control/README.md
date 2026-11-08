# Central nodes on one host with Docker Compose

This directory runs the central nodes of headlamp on one host: `headscale`, `node-control` and, as an option,
`funnel-ingress`. A Caddy container is the TLS edge in front of them. Use it on any Linux host with Docker. For AWS,
[`terraform/modules/control-host/aws`](../../../terraform/modules/control-host/aws/) makes the host (see "AWS" below).

```text
Internet -> load balancer (TCP 443, 80; UDP 3478, no TLS) -> host
              host: edge (Caddy, TLS) -> headscale :8080, node-control :8081, funnel-ingress :8080
                    headscale        <- UDP 3478 (STUN)
```

The load balancer does not end TLS. The edge on the host gets the certificates. One path thus works for headscale,
DERP and the funnel wildcards, and no cloud certificate service is necessary.

## What you need

- A Linux host with Docker and the compose plugin, and a data directory that is not lost when you replace the host.
- Public ports 443/tcp, 80/tcp and 3478/udp to the host. Port 80 is for ACME HTTP-01 and the redirect to HTTPS.
- A DNS name for headscale, for example `headscale.example.com`, that points at the host or at the load balancer.
- For `funnel-ingress` only: a public domain for funnel names, and API access to its DNS zone (see below).

## Files

| File | What it holds | In git? |
|---|---|---|
| `compose.yaml` | The services. Use it without a change. | yes |
| `caddy/Caddyfile`, `caddy/Dockerfile` | The TLS edge: one block for each public name | yes |
| `config/headscale.yaml`, `config/acl.json` | The headscale settings and policy, mounted read-only | yes, in your private repo |
| `config/node-control/` | Optional `nodes.json`, `enroll.json` and `admin.json`, mounted at `/srv/app` | yes, in your private repo |
| `.env` | The image digests, the names and the data directory | no |
| `node-control.env`, `funnel-ingress.env` | The secrets of each app | no |
| `edge.env`, `caddy/secrets/` | Optional: the DNS credentials of the edge, for funnel only | no |

Copy this directory into your private repo. Make each `*.example` file into the file without `.example`, and fill it in.
The `.gitignore` here keeps `.env` and the `*.env` files out of git.

## First start

1. Set `server_url` in `config/headscale.yaml` to `https://<your headscale name>`, and set `dns.base_domain`.
2. Set `HEADSCALE_DOMAIN` and `ACME_EMAIL` in `.env`.
3. Pin each image by version and digest in `.env`. To get the digest, run
   `docker buildx imagetools inspect ghcr.io/valentinalexeev/headlamp/headscale:<version>`.
4. Start headscale and the edge:

   ```sh
   docker compose up -d headscale edge
   ```

5. Make sure that `https://<your headscale name>/health` answers `{"status":"pass"}`.
6. Create a user and the keys for node-control:

   ```sh
   docker compose exec headscale headscale users create _svcs
   docker compose exec headscale headscale preauthkeys create -u <id> --tags tag:node-control -e 1h
   docker compose exec headscale headscale apikeys create -e 90d
   ```

7. Put the pre-auth key and the API key into `node-control.env`, and set `ADMIN_USERS` to your login.
8. Start node-control:

   ```sh
   docker compose up -d node-control
   ```

The node-control console is on the tailnet only, at `http://node-control/`. It has no public name.

## Funnel (optional)

1. Make a pre-auth key with `tag:funnel-ingress`, and put it into `funnel-ingress.env`.
2. Set `FUNNEL_CONFIG_URL` to the tailnet address of node-control.
3. Set `FUNNEL_DOMAIN` in `.env`. A service is then `<service>.<namespace>.<FUNNEL_DOMAIN>`.
4. Point one DNS record, `*.<FUNNEL_DOMAIN>`, at the host or the load balancer. A new namespace needs no new record.
5. Start it:

   ```sh
   docker compose --profile funnel up -d
   ```

### The edge and its certificates

The edge is a Caddy container. `caddy/Dockerfile` builds it from the official `caddy` image. It adds a DNS plugin
only when you ask for one. Caddy gets each certificate from Let's Encrypt with the ACME protocol, and renews it.

All names on the edge are public names on the internet. They are the headscale name, the enrolment name and the
funnel names. Let's Encrypt checks each name in one of two ways:

- HTTP-01. Let's Encrypt connects to the name on port 80, and Caddy answers. This needs only port 80 open and a
  public DNS record. It works on any host, in any cloud, with no plugin and no credentials.
- DNS-01, for a wildcard certificate such as `*.pdl.<FUNNEL_DOMAIN>`. Let's Encrypt does not accept HTTP-01 for a
  wildcard. Caddy must write a TXT record in the public DNS zone, through the API of your DNS provider.

**Funnel uses HTTP-01 by default (`FUNNEL_DNS=ondemand`).** Caddy gets a certificate for each funnel host at the
first TLS connection to it. Before it asks Let's Encrypt, it asks funnel-ingress whether the host is a live route.
funnel-ingress says yes only for `<service>.<namespace>.<FUNNEL_DOMAIN>` with a route. Thus a client cannot make the
edge request certificates for names that you did not publish. A new namespace or service needs no change on the edge.

The first request to a new host waits a few seconds for the certificate. Let's Encrypt limits the number of new
certificates for each domain each week. If you have many services, use one wildcard certificate for each namespace
instead (DNS-01, below).

**node-control can write the public record.** The edge needs one record, `*.<FUNNEL_DOMAIN>`, that points at the
edge. Set these values in `node-control.env` and node-control creates the record. It checks the record every 10
minutes and puts it back if it changed:

| Variable | Value |
|---|---|
| `FUNNEL_INGRESS_HOSTNAME` | The address of the edge (an A or AAAA record) or its name (a CNAME record) |
| `FUNNEL_DNS_PROVIDER` | `route53` or `cloudflare` |
| `FUNNEL_DNS_ZONE` | The ID of the zone of `FUNNEL_DOMAIN` |
| `FUNNEL_DNS_CLOUDFLARE_TOKEN` | Cloudflare only: a token that can edit DNS in that zone |

On AWS, set `route53_zone_id` in the Terraform module. The instance role then gets the necessary rights. The funnel
page in the console shows the status of the record. Without `FUNNEL_DNS_PROVIDER`, the page shows the record to add
manually. On Fly.io, keep the manual checklist, because Fly gets the certificates with `fly certs`.

To use DNS-01 wildcards, set these values in `.env` for the DNS provider of `FUNNEL_DOMAIN`:

| DNS provider | `EDGE_DNS_PLUGINS` | `FUNNEL_DNS` | Credentials in `edge.env` |
|---|---|---|---|
| Route 53 | `github.com/caddy-dns/route53@v1.6.0` | `route53` | none on the AWS host (the instance role); else `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |
| Cloudflare | `github.com/caddy-dns/cloudflare@v0.2.1` | `cloudflare` | `CLOUDFLARE_API_TOKEN` |
| Google Cloud DNS | `github.com/caddy-dns/googleclouddns@v1.1.0` | `googleclouddns` | `GCP_PROJECT`; on a GCE VM nothing else (its service account); else `GOOGLE_APPLICATION_CREDENTIALS` and a key file in `caddy/secrets/` |
| Your own DNS server (BIND, PowerDNS, Knot) | `github.com/caddy-dns/rfc2136@v1.0.0` | `rfc2136` | `RFC2136_KEY_NAME`, `RFC2136_KEY_ALG`, `RFC2136_KEY`, `RFC2136_SERVER` |

Then run `docker compose build edge`. Each value of `FUNNEL_DNS` is a file in `caddy/dns/`. For another provider,
add its plugin to `EDGE_DNS_PLUGINS` and copy one of those files. The Caddy documentation lists the plugins. With
DNS-01, put one line for each namespace, `*.<namespace>.{$FUNNEL_DOMAIN}`, into the funnel block of `caddy/Caddyfile`.

**WARNING:** Give the credentials the right to change the one zone of `FUNNEL_DOMAIN` only. A key that can change
other zones can also take the certificates of other names.

On AWS, the module gives the instance role change rights on the one zone in `route53_zone_id`.

**NOTE:** The edge does not serve the tailnet-only names of serve (#117). Those names are not on the public internet,
so Let's Encrypt cannot check them. node-control signs their certificates with the serve CA.

The edge sends `X-Forwarded-For`. `compose.yaml` thus sets `FUNNEL_CLIENT_IP_HEADER` to `X-Forwarded-For`. It sets
`FUNNEL_TRUSTED_PROXIES` to the compose network, so only the edge can set the client address.

## Enrolment (optional)

Set `ENROLL_DOMAIN` in `.env` to serve the public enrolment links (`/e/<token>` and `/sub/<secret>`) from this host.
The edge sends only these two paths to node-control. Any other path gets 404. Set `ENROLL_DB`, `ENROLL_CONFIG` and
`ENROLL_SECRET_KEY` in `node-control.env` (see the node-control README, "Enrolment").

## AWS

[`terraform/modules/control-host/aws`](../../../terraform/modules/control-host/aws/) makes these parts:

- one EC2 instance (Ubuntu 26.04 LTS Minimal, `t3.small` by default) with Docker and the compose plugin;
- an encrypted EBS data volume, mounted at `/srv/headlamp/data`, with daily snapshots;
- a network load balancer that passes 443/tcp, 80/tcp and 3478/udp to the instance, and keeps the client address;
- a security group with these three ports only, and no SSH unless you set `admin_cidrs`;
- an instance role for SSM Session Manager and, as an option, for DNS-01 on one Route 53 zone.

Example root module, in your private repo:

```hcl
module "control" {
  source          = "github.com/valentinalexeev/headlamp//terraform/modules/control-host/aws?ref=<version>"
  name            = "headlamp-control"
  route53_zone_id = "<zone id, only for funnel wildcards>"
}

output "control_address" { value = module.control.public_address }
```

1. Run `terraform apply`.
2. Point your DNS names at the `public_address` output: a CNAME, or a Route 53 alias with `load_balancer_zone_id`.
3. Open a shell on the host with `aws ssm start-session --target <instance_id>`.
4. Copy your directory to `/srv/headlamp/compose` on the host, then do "First start".

Set `load_balancer = false` to use an Elastic IP on the instance and no load balancer. The ports stay the same.

**CAUTION:** The module sets `prevent_destroy` on the data volume. The volume holds the tailnet. To remove it, first
remove the block from the module code or the volume from the state.

## Operations

### Backups

The data directory holds all the state: the headscale database, the node keys and the data of the apps. On AWS, the
module takes a snapshot of the volume each day and keeps it for 14 days (`snapshot_retain_days`).

To restore on AWS:

1. Stop the apps with `docker compose down`.
2. Remove the old volume from the state: `terraform state rm 'module.control.aws_ebs_volume.data'`.
3. Set `data_snapshot_id` in the module to the snapshot id, and run `terraform apply`.

The old volume stays in the account. Delete it yourself after you make sure that the restore worked.

### Upgrades

1. Read the release notes of the new version.
2. Change the image digests in `.env`.
3. Run `docker compose pull`, then `docker compose up -d`.

If the new version does not work, put the old digests back and run `docker compose up -d`.

**CAUTION:** headscale changes its database on upgrades. Take a snapshot before you upgrade headscale.

### Keys

- The headscale API key of node-control expires. Make a new key, change `node-control.env`, and run
  `docker compose up -d node-control`. Then expire the old key with `headscale apikeys expire`.
- The pre-auth keys are used only at the first start. The nodes keep their identity in the data directory.

## Test

`make test-funnel-tls-e2e` runs the edge with a test ACME server (Pebble) and funnel-ingress. It checks that a
published host gets a certificate at its first request, and that other hosts get none.

`make test-compose-control` runs this directory on the local Docker with the images built from this tree. It uses
the internal CA of Caddy in place of Let's Encrypt. A client then joins the tailnet through the edge and reaches
node-control.

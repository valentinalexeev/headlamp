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
- For `funnel-ingress` only: a public domain for funnel names, and DNS-01 access to its zone (Route 53 in this example).

## Files

| File | What it holds | In git? |
|---|---|---|
| `compose.yaml` | The services. Use it without a change. | yes |
| `caddy/Caddyfile`, `caddy/Dockerfile` | The TLS edge: one block for each public name | yes |
| `config/headscale.yaml`, `config/acl.json` | The headscale settings and policy, mounted read-only | yes, in your private repo |
| `config/node-control/` | Optional `nodes.json`, `enroll.json` and `admin.json`, mounted at `/srv/app` | yes, in your private repo |
| `.env` | The image digests, the names and the data directory | no |
| `node-control.env`, `funnel-ingress.env` | The secrets of each app | no |

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
3. Set `FUNNEL_DOMAIN` in `.env`. Each namespace is then `*.<namespace>.<FUNNEL_DOMAIN>`.
4. For each namespace, add a line `*.<namespace>.{$FUNNEL_DOMAIN}` to the funnel block of `caddy/Caddyfile`.
5. Point a wildcard DNS record for each namespace at the host or the load balancer.
6. Start it:

   ```sh
   docker compose --profile funnel up -d
   ```

The edge gets the wildcard certificates with DNS-01 on Route 53. On AWS, it uses the instance role. Give the role the
zone with `route53_zone_id` in the module. For another DNS provider, change the plugin in `caddy/Dockerfile`.

The edge sends `X-Forwarded-For`. `compose.yaml` thus sets `FUNNEL_CLIENT_IP_HEADER` to `X-Forwarded-For`. It sets
`FUNNEL_TRUSTED_PROXIES` to the compose network, so only the edge can set the client address.

## Enrolment (optional)

Set `ENROLL_DOMAIN` in `.env` to serve the public enrolment links (`/e/<token>` and `/sub/<secret>`) from this host.
The edge sends only these two paths to node-control. Any other path gets 404. Set `ENROLL_DB`, `ENROLL_CONFIG` and
`ENROLL_SECRET_KEY` in `node-control.env` (see the node-control README, "Enrolment").

## AWS

[`terraform/modules/control-host/aws`](../../../terraform/modules/control-host/aws/) makes these parts:

- one EC2 instance (Ubuntu 24.04, `t3.small` by default) with Docker and the compose plugin;
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

`make test-compose-control` runs this directory on the local Docker with the images built from this tree. It uses
the internal CA of Caddy in place of Let's Encrypt. A client then joins the tailnet through the edge and reaches
node-control.

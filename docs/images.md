# Container images and how they fit together

CI publishes six images to `ghcr.io/valentinalexeev/headlamp/<image>`. A few more parts run directly on hosts. Together they make up a self-hosted tailnet: a control server, exit nodes, an admin console and optional public ingress. This page says what each image does, where it runs and what it needs from you.

For tags and release rules, see [`VERSIONING.md`](../VERSIONING.md). The list CI builds from is [`.github/components.json`](../.github/components.json).

## The whole picture

```mermaid
flowchart TB
    internet([Public internet])
    clients([Your devices])

    subgraph tailnet [Tailnet]
        headscale[headscale<br/>control server]
        console[node-control<br/>admin console]
        exit[exit-node]
        vm[VM exit nodes<br/>not an image]
        funnel[funnel-ingress]
        sidecar[funnel-sidecar]
        runner[task-runner on a VM<br/>not an image]
        browser[task-runner-browser<br/>one container per run]
        agent[ingress-agent and xray on a bridge host<br/>not an image]
        guard[tailguard<br/>upstream image]
    end

    clients -->|join and get their policy from| headscale
    clients -->|traffic leaves through| exit
    clients -->|traffic leaves through| vm
    clients -->|admin pages| console
    console -->|node list and policy| headscale
    console -->|config| funnel
    console -->|start runs| runner
    console -->|credentials| agent
    runner --> browser
    internet -->|public names| funnel
    funnel -->|proxies to services on the tailnet| sidecar
    sidecar -->|registers itself| funnel
    guard -->|egress through| exit
```

Every node, image or not, joins the tailnet through headscale. `exit-node`, `node-control`, `funnel-ingress` and `funnel-sidecar` each join as a tailnet node of their own. The diagram leaves those links out, and the cloud provider APIs node-control calls to start and stop VMs.

## Images

| Image | What it does | Where it runs | Needed? |
|---|---|---|---|
| `headscale` | The control server: who is on the tailnet and what they may reach | One container with a public HTTPS name and a volume (a VM such as the AWS control host, a Docker host, a cluster, a Fly.io machine) | Yes |
| `exit-node` | A tailnet exit node, with routes for chosen hostnames that always leave through it | A container with `NET_ADMIN`, `/dev/net/tun` and forwarding on (any Docker host or a Fly.io machine; see its README) | No, if VM exit nodes are enough |
| `node-control` | The admin console and API: start and stop nodes, enrol devices, manage policy, Funnel config and tasks | One machine, reachable only over the tailnet | Strongly recommended, and required by `funnel-ingress` and task-runner |
| `funnel-ingress` | Publishes tailnet services under public names, as a reverse proxy | A public container behind a TLS edge (a reverse proxy such as the Caddy edge in `examples/compose/control/`, a load balancer, or the Fly.io edge) | Only for public ingress |
| `funnel-sidecar` | Registers one service with `funnel-ingress` from Kubernetes, with no special privileges. Published for `linux/amd64` and `linux/arm64` | Beside your workload | Only for Kubernetes services |
| `task-runner-browser` | The Chromium and Playwright container each scheduled browser check runs in | On a VM, started by task-runner under rootless podman | Only for tasks and browser sessions |

### What each one needs

The images are generic. Where a site's settings belong they hold placeholders, or nothing at all, so **none of them runs usefully without your configuration**.

| Image | You supply | State it keeps |
|---|---|---|
| `headscale` | `headscale.yaml` and `acl.json`: mount them and set `HEADSCALE_CONFIG`, or replace the placeholders in the image | Its database, on a volume |
| `exit-node` | Auth key, `direct-hosts.txt` (mount it and set `DIRECT_HOSTS_FILE`), and optional state encryption settings | Tailscale identity, on a volume or encrypted in Redis |
| `node-control` | Environment settings, cloud credentials, `headscale` API key, and optional `nodes.json` and `enroll.json` (mount them and set `NODES_CONFIG` and `ENROLL_CONFIG`) | Enrolment, policy and tasks data, in files or Redis depending on what you enable |
| `funnel-ingress` | Environment settings and an auth key | Its tailnet identity and registered routes; namespaces and blocks come from node-control |
| `funnel-sidecar` | Environment settings naming the namespace and service | None: its node is ephemeral |
| `task-runner-browser` | Nothing: the caller sends each script | None: each container is thrown away |

Each app's README has the full settings.

## Parts that are not images

| Part | How it runs | Why it has no image |
|---|---|---|
| VM exit nodes | Terraform creates them, and the Ansible roles configure them | They are machines, not containers |
| `ingress-agent` | A systemd service, installed by Ansible on a bridge host | It restarts xray through systemd and owns xray's config on the host |
| `task-runner` | A binary on a VM, installed by Ansible | It starts containers itself, under its own locked-down user |
| `tailguard` | Docker Compose, run by you | It uses the upstream `ghcr.io/juhovh/tailguard` image plus scripts from this repo |

CI still runs the tests for `ingress-agent` and `task-runner`.

## How a request flows

**A device using an exit node.** The device joins through `headscale`, which gives it the policy and the list of peers. Its traffic then goes directly to the exit node over WireGuard. `headscale` carries no user traffic.

**Starting a node from the console.** Over the tailnet, you open `node-control`, which calls the cloud provider to start the VM and checks `headscale` to see when it is back online.

**A public request.** A visitor reaches `funnel-ingress` at `<service>.<namespace>.<domain>`. It looks up the route and proxies over the tailnet to the node serving it. That node may be a `funnel-sidecar`, or any tagged node that registered itself. `node-control` holds the namespaces and blocks, and `funnel-ingress` pulls them.

**A scheduled check.** `node-control` sends a script to `task-runner` on a VM. `task-runner` starts a throwaway `task-runner-browser` container, runs the script, and returns a small JSON result.

## Using the images

Your settings stay in your private repo, as with any other part of this project. There are two ways to give them to an
image.

### Mount the settings at runtime

Use the published image without a change. Mount your files read-only and set the variable that names each file:

| Image | Variable | Default (the placeholder in the image) |
|---|---|---|
| `headscale` | `HEADSCALE_CONFIG` | `/etc/headscale/config.yaml` |
| `headscale` | `HEADSCALE_POLICY_PATH` (optional, overrides `policy.path` in the config) | `/etc/headscale/acl.json` |
| `exit-node` | `DIRECT_HOSTS_FILE` | `/app/direct-hosts.txt` |
| `node-control` | `NODES_CONFIG`, `ENROLL_CONFIG` | `/srv/app/nodes.json`, no default |

Example:

```bash
docker run -d --name headscale \
  -v ./config:/config:ro -v headscale-data:/data \
  -e HEADSCALE_CONFIG=/config/headscale.yaml -e HEADSCALE_POLICY_PATH=/config/acl.json \
  ghcr.io/valentinalexeev/headlamp/headscale:0.9.0@sha256:<digest> serve
```

The `headscale` image is the stock, distroless headscale image with the placeholder settings. Its entrypoint is the
`headscale` binary, so give only the arguments (`serve`). The image has no shell. Use `docker exec <container> headscale
<command>` to run a command in it.

Pin the digest as well as the version. If you do not set a variable, the image uses its placeholder.

### Add the settings in a thin image

A thin Dockerfile copies your files on top of the image and keeps the deployment pinned:

```dockerfile
FROM ghcr.io/valentinalexeev/headlamp/node-control:1.2.3@sha256:<digest>
COPY app ./app
```

Pin the digest as well as the version.

Two things do not use the published images yet:

- `fly/deploy.sh` in [`examples/private-repo/`](../examples/private-repo/) still builds from source, with your files laid over the app directory. That keeps working.
- The `task-runner` Ansible role builds the browser image on each node, or loads a `podman save` archive for nodes that cannot reach a registry. Set `task_runner_base_image` for the first, or `task_runner_image_archive` for the second.

All images are `linux/amd64` for now.

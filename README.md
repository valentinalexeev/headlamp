# headlamp

Infrastructure code for a self-hosted, Tailscale-compatible network:

- **headscale** control server (`apps/headscale`)
- **central nodes on any host**: headscale, node-control and funnel-ingress as published container images. Run them on
  AWS (`terraform/modules/control-host/aws` with `examples/compose/control/`), on any Docker host with the same
  compose files, or on Fly.io (a `fly.toml` per app)
- **exit nodes**: a container image (`apps/exit-node`) and VM-based nodes on Yandex Cloud, Oracle
  Cloud, AWS, Cloud.ru or OpenStack clouds such as Selectel. The directory `terraform/modules/exit-node-vm` creates the VM nodes. The Ansible roles in `ansible/` configure them.
- **split-tunnel route provisioning**: nodes advertise routes for chosen ASNs/hostnames so those
  services leave from a chosen country (`ansible/roles/route-provisioner`)
- **node-control** (`apps/node-control`): a small web UI, only on the tailnet, to start and stop the VM nodes.
  It can also schedule browser-script checks. These run on a node through **task-runner** (`apps/task-runner`,
  `ansible/roles/task-runner`). Its optional tailnet DNS module answers tailnet names: zones, A, AAAA, CNAME, TXT
  and SRV records, and answers per user or tag. See [`docs/dns.md`](docs/dns.md).
- **funnel-ingress**: publishes tailnet services under stable public names, Funnel-style (`apps/funnel-ingress`)
- **tailguard** (`apps/tailguard`): a WireGuard server that sends its traffic out through a tailnet exit node.
  A plain-WireGuard device, for example a home router, can then use your exit nodes.
  It has a watchdog sidecar and a sidecar that follows the exit node.
- a workflow for keeping **Terraform state as a sops-encrypted file in a private git repo**
  (`scripts/tf-run.sh`, `.githooks/`)

## This repo is generic on purpose

It contains no secrets, no state, and nothing that identifies a deployment: every site-specific
value is a placeholder (`example.com`, `<your-...>`). Run it from **your own private repo**.
The private repo vendors this repo as a git submodule (`vendor/headlamp`). It holds your Terraform root
stacks, encrypted state, Ansible inventory, variables and vault, the config of your container host and route lists.

**Start with [`CLAUDE.md`](CLAUDE.md)** (also readable by humans): the repo rules and a step-by-step
bootstrap guide. [`examples/private-repo/`](examples/private-repo/) is the skeleton to copy.

## Images and releases

CI publishes the components that run as containers to GHCR. They run on any container host. These targets are
ready to use, and none of them is the default:

- **AWS:** `terraform/modules/control-host/aws` builds the host, and `examples/compose/control/` runs the central nodes on it.
- **Any Docker host:** the same compose files, with the published images and your config mounted.
- **Fly.io:** a `fly.toml` per app and an overlay deploy in `examples/private-repo/`. [`docs/images.md`](docs/images.md) explains what each image does, how they fit together and what each needs from its host, and [`VERSIONING.md`](VERSIONING.md) covers tags and release rules. [`UPGRADING.md`](UPGRADING.md) lists what a site repo must change for each release.

Each release also publishes the Helm chart [`charts/tailnet-connector`](charts/tailnet-connector/README.md) to
`oci://ghcr.io/valentinalexeev/headlamp/charts`. The chart connects a Kubernetes cluster to the tailnet.

## Terraform state workflow

State lives in your private repo as `terraform/terraform.tfstate.enc.json` (sops + age). Git has
no locking or versioning of its own, so `scripts/tf-run.sh` (run from your private repo's root)
enforces the discipline:

- `apply` refuses to run from a dirty `terraform/` or from a checkout behind its upstream
  (`ALLOW_DIRTY=1` and `ALLOW_STALE=1` override the checks). You cannot apply on stale state.
- The script verifies the new state (semantic round-trip decrypt) and installs it. It then **commits and pushes** the state
  right after every run that changed it. This includes a failed run and a run that you stop with Ctrl-C. A plain `plan`
  leaves the encrypted file byte-identical.
- If anything fails the plaintext state is **kept**, never deleted, and the next run refuses to
  start until you run `tf-run.sh save` (or delete it).
- The pre-commit hook (`.githooks/pre-commit`) rejects plaintext state/secrets, unencrypted
  `*.enc.*` files and private-key-shaped strings. `.gitattributes` marks encrypted files binary.
- Keep a second age key offline: with one key, losing it loses the state and every secret.
- Tests: `make test-tf-run` (stub sops/terraform, throwaway repos).

## Development

`make hooks` enables the commit hook. `make test-tf-run` runs the state-wrapper tests. CI runs these checks on each
pull request, and you can run them before you push:

| Target | Check |
|---|---|
| `make lint` | shellcheck, warnings included |
| `make lint-go` | golangci-lint on new and changed Go code ([ADR 0002](docs/adr/0002-adopt-a-subset-of-tigerstyle.md)) |
| `make lint-go-report` | golangci-lint on all Go code, as warnings |
| `make vulncheck` | govulncheck on each Go module |
| `make lint-config` | actionlint, hadolint and kubeconform |
| `make check-tailscale-pins`, `make check-headscale-pin` | the version pins agree with `TAILSCALE_VERSION` and `HEADSCALE_VERSION` | See `CLAUDE.md` for the rules about what may never be committed here.

Third-party notices: [`THIRD_PARTY.md`](THIRD_PARTY.md). License: [MIT](LICENSE).

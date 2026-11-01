# headlamp

Infrastructure code for a self-hosted, Tailscale-compatible network:

- **headscale** control server on Fly.io (`apps/fly/headscale`)
- **exit nodes**: a Fly machine (`apps/fly/exit-ams`) and VM-based nodes on Yandex Cloud, Oracle
  Cloud, AWS, Cloud.ru or Selectel. The directory `terraform/modules/exit-node-vm` creates the VM nodes. The Ansible roles in `ansible/` configure them.
- **split-tunnel route provisioning**: nodes advertise routes for chosen ASNs/hostnames so those
  services leave from a chosen country (`ansible/roles/route-provisioner`)
- **node-control** (`apps/fly/node-control`): a small web UI, only on the tailnet, to start and stop the VM nodes.
  It can also schedule browser-script checks. These run on a node through **task-runner** (`apps/task-runner`,
  `ansible/roles/task-runner`).
- **funnel-ingress**: publishes tailnet services under stable public names, Funnel-style (`apps/fly/funnel-ingress`)
- **tailguard** (`apps/tailguard`): a WireGuard server that sends its traffic out through a tailnet exit node.
  A plain-WireGuard device, for example a home router, can then use your exit nodes.
  It has a watchdog sidecar and a sidecar that follows the exit node.
- a workflow for keeping **Terraform state as a sops-encrypted file in a private git repo**
  (`scripts/tf-run.sh`, `.githooks/`)

## This repo is generic on purpose

It contains no secrets, no state, and nothing that identifies a deployment: every site-specific
value is a placeholder (`example.com`, `<your-...>`). Run it from **your own private repo**.
The private repo vendors this repo as a git submodule (`vendor/headlamp`). It holds your Terraform root
stacks, encrypted state, Ansible inventory, variables and vault, Fly configs and route lists.

**Start with [`CLAUDE.md`](CLAUDE.md)** (also readable by humans): the repo rules and a step-by-step
bootstrap guide. [`examples/private-repo/`](examples/private-repo/) is the skeleton to copy.

## Images and releases

CI publishes the components that run as containers to GHCR. [`docs/images.md`](docs/images.md) explains what each image does and how they fit together, and [`VERSIONING.md`](VERSIONING.md) covers tags and release rules.

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

`make hooks` enables the commit hook; `make lint` runs shellcheck; `make test-tf-run` runs the
state-wrapper tests. See `CLAUDE.md` for the rules about what may never be committed here.

Third-party notices: [`THIRD_PARTY.md`](THIRD_PARTY.md). License: [MIT](LICENSE).

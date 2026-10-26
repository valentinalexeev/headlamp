# headlamp

Infrastructure code for a self-hosted, Tailscale-compatible network:

- **headscale** control server on Fly.io (`apps/fly/headscale`)
- **exit nodes**: a Fly machine (`apps/fly/exit-ams`) and VM-based nodes on Yandex Cloud, Oracle
  Cloud, AWS, Cloud.ru or Selectel (`terraform/modules/exit-node-vm`, configured by the Ansible roles in `ansible/`)
- **split-tunnel route provisioning**: nodes advertise routes for chosen ASNs/hostnames so those
  services leave from a chosen country (`ansible/roles/route-provisioner`)
- **node-control**: a small tailnet-only web UI to start/stop the VM nodes (`apps/fly/node-control`), with optional
  scheduled browser-script checks that run on a node through **task-runner** (`apps/task-runner`,
  `ansible/roles/task-runner`)
- **funnel-ingress**: publishes tailnet services under stable public names, Funnel-style (`apps/fly/funnel-ingress`)
- **tailguard** (`apps/tailguard`): a WireGuard server that egresses through a tailnet exit node,
  so a plain-WireGuard device (e.g. a home router) can use your exit nodes, with watchdog and
  exit-node-following sidecars
- a workflow for keeping **Terraform state as a sops-encrypted file in a private git repo**
  (`scripts/tf-run.sh`, `.githooks/`)

## This repo is generic on purpose

It contains no secrets, no state, and nothing that identifies a deployment: every site-specific
value is a placeholder (`example.com`, `<your-...>`). You run it from **your own private repo**
that vendors this one as a git submodule (`vendor/headlamp`) and holds your Terraform root
stacks, encrypted state, Ansible inventory/vars/vault, Fly configs and route lists.

**Start with [`CLAUDE.md`](CLAUDE.md)** (also readable by humans): the repo rules and a step-by-step
bootstrap guide. [`examples/private-repo/`](examples/private-repo/) is the skeleton to copy.

## Terraform state workflow

State lives in your private repo as `terraform/terraform.tfstate.enc.json` (sops + age). Git has
no locking or versioning of its own, so `scripts/tf-run.sh` (run from your private repo's root)
enforces the discipline:

- `apply` refuses from a dirty `terraform/` or a checkout behind its upstream
  (`ALLOW_DIRTY=1` / `ALLOW_STALE=1` override), so you can't apply on stale state.
- The new state is verified (semantic round-trip decrypt), installed, **committed and pushed**
  right after every run that changed it, including a failed or Ctrl-C'd one. A plain `plan`
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

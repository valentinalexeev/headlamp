# headlamp — instructions for agents and operators

headlamp is infrastructure code for a self-hosted Tailscale-compatible network: a
**headscale** control server on Fly.io, **exit nodes** (a Fly machine and VM-based nodes on
Yandex / OCI / AWS), split-tunnel route provisioning, a small **node-control** web app that
starts/stops the VM nodes, and **tailguard** (a WireGuard server that egresses through a
tailnet exit node, so a plain-WireGuard device such as a home router can use it).

This file tells you (a) the rules for changing this repo, and (b) how to bootstrap your own
infrastructure from it with **your own private repo that vendors this one as a git
submodule**. Read `README.md` for an overview.

## 1. The two-repo model (read this first)

```
headlamp            PUBLIC   generic, reusable code only. No secrets, no state, no one's real
                             hostnames/IDs/route lists. This repository.
<your>-infra        PRIVATE  your site: Terraform root stacks + encrypted state, Ansible
                             inventory/vars/vault, Fly configs, headscale config + ACL, route
                             lists, secrets. Contains headlamp as a submodule at vendor/headlamp.
```

Dependencies point one way: the private repo uses headlamp; headlamp never refers to a private
repo. Everything that identifies a deployment lives only on the private side.

### Rules when editing THIS (public) repo

- **Never commit** secrets, keys, tokens, plaintext or encrypted Terraform state, `.env` files,
  real domains/hostnames, tailnet names, cloud account/folder/compartment/instance IDs,
  password-manager item IDs, personal IPs, or personal route/host lists. Use placeholders:
  `example.com`, `203.0.113.0/24`, `<your-fly-app>`, `<your-compartment-ocid>`.
- Generic > specific. If you need a value, make it a variable/env/default with a placeholder,
  and document where the real one goes (the private repo).
- A pre-commit hook enforces the mechanical part (`make hooks`; the tf targets do it for you).
  It is a backstop, not a substitute for judgement. Run a quick scan of what you staged:
  `git diff --cached | grep -iE 'ocid1|tskey|hskey|AGE-SECRET|BEGIN .*PRIVATE'`.
- Do not rewrite or force-push public history: private repos pin commits of it. The only exception is an
  explicit operator instruction to remove a confidentiality leak (see the next rule): then squash to a single
  new root commit, bump every private repo's submodule pin, delete all other branches and tags, and tell the
  operator that the hosting platform keeps old objects reachable (pull-request refs, caches, forks) until
  the platform purges them or the repository is recreated.
- Do not push, deploy, apply, or destroy anything unless the operator asked for exactly that.
  `terraform apply`, `flyctl deploy`, `flyctl apps destroy`, and Ansible runs against real hosts
  change real infrastructure.

### No geographic or political references (hard rule)

The tooling here must read as generic infrastructure for **restrictive networks** (networks that block or
throttle many destinations). It must never reveal where it is used or for whom. Everywhere in this repository
-- code, comments, docs, tests, fixtures, example values, identifiers, file/branch/tag names, commit
messages, PR and issue titles and bodies -- do **not** write:

- names or codes of countries, regions, cities, languages or nationalities; carrier/ISP names; names of
  governments, regulators or blocking/filtering systems; or any wording that frames hosts or traffic by place;
- place-coded identifiers (node names such as `node-<country code>`, zone ids copied from a real region,
  `geoip:<real country code>` examples). Use `xx`, `<zone>`, `<region>`, `example` placeholders.

Describe a capability by its mechanism and a generic purpose: "a relay reachable from a client's restricted
network", "destinations on configured *direct lists* leave from the relay host (some services only accept
that network), everything else uses the exit node". Name things by what they do (`direct`, `exit`, `relay`,
`ingress`). Which lists, regions and providers a deployment uses belongs only in the private site repo.
Cloud-provider names may appear only as the identifier of the module/integration itself, never in prose about
where or why it is used.

Why: this repository is world-readable and the people who rely on this tooling can be put at risk by a
public association with a place or an authority.

Enforcement: `scripts/check-denylist.sh` (run by the `pre-commit` and `commit-msg` hooks; `make hooks`)
checks added lines, paths and messages against a pattern list that lives **outside** the repo
(`$HEADLAMP_DENYLIST` or `~/.config/headlamp/denylist`). That list must never be committed, quoted in a
PR/issue/commit message, or reconstructed in any file here. Agents: set `HEADLAMP_DENYLIST_REQUIRED=1`, run
`scripts/check-denylist.sh --staged` before every commit, and `scripts/check-denylist.sh --text FILE` on every
commit message, branch name, PR title/body and issue text *before* it leaves the machine. If the list is
missing, stop and ask the operator. Fix a hit by rewriting the sentence generically, not by swapping in a
synonym or abbreviation. If something slipped into history, tell the operator; do not push more
commits on top of it or attempt a rewrite on your own.
- Commit style: Conventional Commits with a scope (`feat(tailguard): …`, `fix(exit-ams): …`,
  `docs: …`). Keep commits focused.
- Verify before claiming something works: `make test-tf-run`; `shellcheck` on shell scripts;
  `terraform fmt -check && terraform validate` for module changes; `ansible-playbook
  --syntax-check` for roles; `docker build` for app images. Say what you ran and what you did not.

### Repo map

| Path | What it is |
|---|---|
| `terraform/modules/exit-node-vm/{yandex,oci,aws,cloudru,selectel}` | reusable VM exit-node modules (root stacks live in YOUR repo) |
| `ansible/roles/{tailscale,route-provisioner,task-runner}`, `ansible/playbooks/`, `ansible/templates/` | install/join the tailnet; split-tunnel routes; the task runner on a VM node (rootless podman, nftables guard for its user; see the role README); version bump / key rotation playbooks |
| `apps/fly/headscale` | headscale server image; `headscale.yaml`, `acl.json`, `fly.toml` are **placeholders**. The route approver (`approve-gateway-routes.sh`, `cron-approve-routes.sh`) is shipped but OFF by default (commented `route_approver` process in `fly.toml`); prefer `autoApprovers.routes` in `acl.json` |
| `apps/fly/exit-ams` | Fly tailscale exit node; `fly.toml`, `direct-hosts.txt` are **placeholders** |
| `apps/fly/node-control` | Go service (embedded tailscale): start/stop UI and API for the exit nodes (discovered from headscale), tailnet status, optional device enrolment, optional policy console, optional scheduled browser-script tasks (`internal/tasks`, see its README section "Tasks module"); `fly.toml` is a **placeholder**, `app/nodes.json` is an optional overlay |
| `apps/fly/funnel-ingress` | Go service (embedded tailscale): public name-based reverse proxy into the tailnet (`<service>.<ns>.<domain>`), tag-authorised self-registration API, a Kubernetes registration sidecar (`funnel-sidecar`), config pulled from node-control; `fly.toml` is a **placeholder** |
| `apps/task-runner` | Go service run on a node (by the `task-runner` role): runs one caller-supplied browser script at a time in a throwaway rootless container and returns a small JSON result; called by node-control over the tailnet |
| `apps/tailguard` | WireGuard server → tailnet exit node (Docker, operator-run) |
| `scripts/tf-run.sh`, `.githooks/`, `scripts/test/` | state-in-git workflow, commit hook, tests |
| `scripts/headscale-assign-host.sh` | helper to register a host under an existing pre-auth key (`HEADSCALE_APP`, `HEADSCALE_LOGIN_SERVER` env) |
| `scripts/rotate-headscale-api-key.sh` | check (`--check`) and rotate the headscale API key used by the route approver and other apps; all site values come from env vars (`HS_APP`, `HS_URL`, `STAGE_APPS`, `DEPLOY_CMD`, optional sops `SECRET_FILE`/`SECRET_FIELD`). API keys expire: the approver logs `ERROR ... HTTP 401` when its key has, and keeps retrying |
| `scripts/tasks.sh`, `scripts/test/tasks/` | CLI client of the node-control tasks API: check/diff/push (secrets first, from a sops file)/pull the task config and scripts (env: `NODE_CONTROL_URL`, `TASKS_FILE`, `TASKS_SCRIPTS_DIR`, `TASKS_SECRETS_FILE`); tests run against fake `curl` and `sops` (`make test-tasks-cli`) |
| `scripts/headscale-policy.sh` | CLI client of the node-control policy console: check/diff/apply (with confirm-or-revert)/pull the headscale ACL; `bootstrap` loads the first policy through `fly ssh` (env: `NODE_CONTROL_URL`, `POLICY_FILE`, `HS_APP`) |
| `scripts/headscale-tag-node.sh` | show, add or remove an admin-held tag on one headscale node by its given name, keeping its other tags (the API replaces the whole set). Prints the tag model: unified (headscale 0.28+, one `tags` list; tagging a user-owned node drops its user ownership, so that needs `--allow-ownership-change`) or legacy (only forced tags change). Plan without `--yes`, verify after applying (env: `HEADSCALE_URL`, `HEADSCALE_API_KEY` or `HEADSCALE_API_KEY_CMD`). Used to tag task runners |
| `examples/private-repo/` | the skeleton of the private site repo (Makefile, terraform, ansible, fly deploy) |

Anything that identifies a deployment is a placeholder here (`example.com`, `<your-...>`). The
Dockerfiles `COPY` those placeholder files, so a forked build works but is not configured: your
private repo lays its real files over them at deploy time (section 2.5).

## 2. Bootstrap your own infra

You will end up with: a private repo `<your>-infra` containing your config and encrypted state,
with this repo at `vendor/headlamp`. Do the steps in order and stop to confirm before anything
that costs money or touches a real account.

### 2.0 Prerequisites

- Accounts: Fly.io (required, for headscale), and at least one VM provider if you want VM exit
  nodes (Yandex Cloud / Oracle Cloud / AWS). A domain you control is recommended
  (MagicDNS base domain, stable node names).
- Tools: `git`, `sops`, `age`, `terraform` (>= 1.5), `ansible` (+ collections, below), `flyctl`,
  `jq`, `python3`, `shellcheck`; plus the provider CLIs you use (`yc`, `oci`, `aws`). Optional:
  1Password CLI `op` (otherwise supply `SOPS_AGE_KEY` from a file/keychain, see 2.3).
- Dedicated, narrowly scoped cloud credentials per provider (a separate CLI profile such as
  `~/.oci/config [headlamp]`), never your everyday identity.

### 2.1 Create the private repo and vendor headlamp

```bash
mkdir my-infra && cd my-infra && git init -b main
git submodule add https://github.com/<owner>/headlamp.git vendor/headlamp
git config submodule.recurse true
git config push.recurseSubmodules check    # refuse to push a pointer to an unpushed submodule commit
git commit -m "chore: vendor headlamp"
# create the remote as PRIVATE, then:
git remote add origin git@github.com:<you>/my-infra.git && git push -u origin main
```

Rules for the submodule: never edit files under `vendor/headlamp` as part of site work — a
change to headlamp is made in a clone of the public repo, pushed there, then pulled in by bumping
the pointer. Anyone cloning your private repo uses `git clone --recurse-submodules`
(or `git submodule update --init`).

### 2.2 Private repo layout

Start from `vendor/headlamp/examples/private-repo/` (copy it, then edit); it matches this layout.

```
my-infra/
  vendor/headlamp/            # submodule (this repo)
  .sops.yaml                  # age recipients (section 2.3)
  .gitattributes              # copy from vendor/headlamp/.gitattributes
  Makefile                    # glue (section 2.3)
  terraform/                  # root stacks, tfvars, ENCRYPTED state (section 2.6)
  ansible/                    # ansible.cfg, inventory, group_vars, host_vars, site.yml (2.7)
  fly/                        # fly.toml + config overlays per Fly app (2.4, 2.5)
  secrets/                    # *.sops.yaml you create (never plaintext)
  docs/                       # your notes
```

### 2.3 Secrets: age key, sops, Makefile glue

```bash
age-keygen -o ~/age-main.txt            # keep this file OUT of the repo; store in a password manager
age-keygen -o /safe/offline/age-backup.txt   # second key, kept offline: losing the only key loses all state+secrets
```

`.sops.yaml` — every rule must list the **same** recipients (`tf-run.sh` encrypts to one set):

```yaml
creation_rules:
  - path_regex: .*\.(sops|enc)\.(yaml|json)$
    age: age1<main-public-key>,age1<backup-public-key>
```

Make the private key available as `SOPS_AGE_KEY` (env var holding the key text). With 1Password:
`export SOPS_AGE_KEY := $(shell op read "op://<vault>/<item>/<field>")` in the Makefile (fetched
once per `make`). Without: `export SOPS_AGE_KEY := $(shell cat ~/age-main.txt)`.

Minimal private `Makefile`:

```make
export SOPS_AGE_KEY := $(shell op read "op://<vault>/<item>/<field>")   # or cat a key file
export TF_DIR := terraform
H := vendor/headlamp
.PHONY: hooks tf-plan tf-apply tf-save
hooks:     ; @git config core.hooksPath $(H)/.githooks
tf-plan:  hooks ; $(H)/scripts/tf-run.sh plan  $(ARGS)
tf-apply: hooks ; $(H)/scripts/tf-run.sh apply $(ARGS)
tf-save:  hooks ; $(H)/scripts/tf-run.sh save
```

Run `tf-run.sh` from the private repo root (it uses the git toplevel of your current directory).
To create an encrypted secrets file: write the plaintext to a temp path **outside the repo**,
`sops -e --filename-override secrets/<name>.sops.yaml <tmp> > secrets/<name>.sops.yaml`
(`--filename-override` makes the creation rules match the destination name, not the temp
path), then delete the temp. The hook refuses plaintext secret files and unencrypted `*.enc.*`/`*.sops.*` files.

### 2.4 headscale control server (Fly.io)

1. Copy `vendor/headlamp/apps/fly/headscale/{fly.toml,headscale.yaml,acl.json}` into
   `fly/headscale/` and edit: Fly app name, `server_url`, `base_domain` (must differ from
   `server_url`'s domain), and the ACL (`tagOwners`, `autoApprovers.exitNode`) for your users/tags.
2. Create the app and its persistent volume, then deploy with the overlay procedure (2.5):
   `flyctl apps create <app>` · `flyctl volumes create headscale_data -a <app> -r <region> -n 1`.
3. Create a user and pre-auth keys on the server
   (`flyctl ssh console -a <app> -C "headscale users create <user>"`, then
   `headscale preauthkeys create …` — flags differ between headscale versions, check `--help`).
   Use reusable, non-ephemeral keys for exit nodes. Keep keys only in sops-encrypted files or
   `flyctl secrets`.

### 2.5 Deploying Fly apps from the submodule (overlay procedure)

The Fly Dockerfiles `COPY` some site files from the app directory (`headscale.yaml`, `acl.json`,
`direct-hosts.txt`, `app/nodes.json`, `fly.toml`), so the build context needs your files while
the submodule must stay pristine. `examples/private-repo/fly/deploy.sh` does exactly that: it
refuses to run if `vendor/headlamp` is dirty, copies `fly/<app>/` over
`vendor/headlamp/apps/fly/<app>/`, runs `flyctl deploy` there, and restores the submodule on exit
(even on failure).

```bash
fly/deploy.sh headscale
fly/deploy.sh exit-ams --build-arg TSVERSION=<x.y.z>
fly/deploy.sh headscale --build-arg HEADSCALE_VERSION=<tag>   # default is pinned in the Dockerfile
```

The headscale image tag is pinned on purpose: `jauderho/headscale:latest` stopped shipping a shell in
v0.29.5, which breaks the Dockerfile's `apk add` step. Check the new tag has `/bin/sh` before bumping.

Deploying is a real change: confirm with the operator first. `git -C vendor/headlamp status` must
be clean afterwards.

### 2.6 VM exit nodes (Terraform) and state

In `terraform/` of the private repo write a root stack that uses the modules by path:

```hcl
module "node1" {
  source                 = "../vendor/headlamp/terraform/modules/exit-node-vm/oci"  # or yandex / aws
  name                   = "node1"                    # also its tailscale hostname
  compartment_id         = var.oci_compartment_id     # no defaults for account IDs in public code
  shape                  = "VM.Standard.E2.1.Micro"
  ocpus                  = 1
  memory_in_gbs          = 1
  ssh_public_key         = var.ssh_public_key
  headscale_login_server = "https://<your-headscale-url>"
  tailscale_authkey      = var.node1_tailscale_authkey   # sensitive
}
```

Plus `providers.tf` (dedicated profiles, no inline credentials), `versions.tf`, `variables.tf`,
and secrets in `terraform/secrets.auto.tfvars.enc.json` (sops-encrypted JSON of tfvars).
Module inputs are documented in each module's `variables.tf`; the Yandex module additionally
takes `folder_id`, `zone`, `subnet_id`, `image_id`, sizing, `user_data`, `extra_metadata`.

**State is a sops-encrypted file in git** (`terraform/terraform.tfstate.enc.json`) handled by
`tf-run.sh`; there is no remote backend. Always use `make tf-plan` / `make tf-apply`, never bare
`terraform` in that directory. What the wrapper guarantees and how to recover: see
`README.md` → "Terraform state workflow". In short: apply refuses from a dirty/stale checkout;
state is verified, committed and pushed after every run that changed it (even a failed one);
plaintext state is never deleted on error, and the next run refuses until `make tf-save`.
First run with no state yet works: apply creates and commits it. Keep one operator machine and
a private remote. Merge conflicts in the encrypted file cannot be resolved by hand — take one
side and re-plan.

### 2.7 Ansible (tailscale join + split-tunnel routes)

```ini
# ansible/ansible.cfg
[defaults]
inventory = inventory/hosts.yml
host_key_checking = False
roles_path = ../vendor/headlamp/ansible/roles
vars_plugins_enabled = host_group_vars, community.sops.sops
```

- Install collections: `ansible-galaxy collection install -r vendor/headlamp/ansible/requirements.yml`.
- `inventory/hosts.yml`: group `exit_nodes_vm` (and `route_provisioned` for nodes that get route
  lists) with `ansible_host` (stable MagicDNS name) and `ansible_user`.
- `site.yml`: apply role `tailscale` to `exit_nodes_vm` and `route-provisioner` to
  `route_provisioned` (copy the structure of `vendor/headlamp/ansible/site.yml`, minus the
  node-control rendering play unless you use node-control).
- `group_vars/all/main.yml`: `headscale_login_server`, `tailscale_version`.
  `group_vars/all/vault.sops.yaml` (encrypted): per-host auth keys.
- `host_vars/<host>.yml`: `tailscale_hostname`, `tailscale_authkey`, optional
  `tailscale_advertise_exit_node`, and the route lists `route_asns`, `route_hosts`,
  `route_hosts_direct` (see `ansible/roles/route-provisioner/defaults/main.yml` for what each does
  and when to prefer `route_hosts_direct` — large shared ASNs like AWS/Akamai must NOT go in
  `route_asns`).
- Run from `ansible/` with `SOPS_AGE_KEY` set:
  `ansible-playbook site.yml --extra-vars tailscale_version=<x.y.z>` (add `--check --diff` first).

### 2.8 Fly exit node and node-control (optional)

- `apps/fly/exit-ams`: copy `fly.toml`, `direct-hosts.txt` to `fly/exit-ams/`; set your app name;
  `flyctl volumes create tailscale_state …`; `flyctl secrets set TAILSCALE_AUTH_KEY=… HEADSCALE_LOGIN_SERVER=…`;
  deploy via 2.5. Its `direct-hosts.txt` is a list of hostnames resolved to `/32` routes
  advertised through that node — yours, not the example.
- `apps/fly/node-control`: a Go service (one binary, embedded tailscale) with a start/stop UI and API for your exit nodes,
  reachable only over the tailnet by design (no public service). Exit nodes are discovered from headscale: a node with
  `tag:exit` is listed, and a `tag:cloud-<provider>` tag (`yandex`, `oci`, `selectel`, `fly`) plus its reported name
  selects the VM or app to control. The optional JSON `app/nodes.json` (see `testdata/nodes.example.json`; keep the real file in
  your private repo and lay it over the app dir with the overlay procedure) only adds overrides: explicit connection
  blocks, labels, aliases, `hidden`. Provider credentials are Fly secrets (`YANDEX_SA_KEY_JSON`, `FLY_API_TOKEN`, the
  OCI keys, `SELECTEL_*`, `HEADSCALE_API_KEY`). Tailnet identity: keep it on the `tailscale_state` volume (see its
  `fly.toml`), or in a shared Redis with `REDIS_URL` and an encryption key. Details: `apps/fly/node-control/README.md`.
- Tasks module (optional, needs that Redis): scheduled browser-script checks that run on a node, with retries, alerts,
  metrics and a heartbeat. Set `REDIS_URL`, `NODE_CONTROL_STATE_KEY`, `TASKS_RUNNER_TOKEN`, `TASKS_ADMIN_USERS` and
  `ENROLL_ALLOWED_HOSTS` (required by the module), deploy the `task-runner` role (`ansible/roles/task-runner`) to the nodes
  that should run scripts, give those nodes the runner tag (`TASKS_RUNNER_TAG`, default `tag:task-runner`; untagged nodes
  are never used, and a `node` target means the headscale given name) as an admin (`scripts/headscale-tag-node.sh`),
  list only the admin/service identity as owner of that tag in the policy's `tagOwners` (it decides who may have the
  tag at registration, and on pre-0.28 headscale a node can also request one later, so `tagOwners` is the trust
  boundary; on 0.28+ tagging a user-owned node drops its user ownership), allow node-control to reach the runner port of
  that tag in the tailnet ACL, and manage config, scripts and secrets from your private repo with `scripts/tasks.sh`
  (example files: `examples/private-repo/fly/node-control/tasks/`). Variables, API, metrics and the trust model:
  `apps/fly/node-control/README.md`, section "Tasks module".

### 2.9 tailguard (optional, operator-run)

For a Docker host that should offer a WireGuard server routed through an exit node: copy
`vendor/headlamp/apps/tailguard/` to the host, create `.env` from `.env.example` and
`config/wg0.conf` from the example, `docker compose up -d`. It is documented end to end in
`apps/tailguard/README.md` (key generation, router client config, verification script, the
watchdog and exit-node-tracker sidecars). Its state (`.env`, `config/wg0.conf`, `state/`) lives
on the host only and is gitignored — keep it out of both repos.

### 2.10 Verify

- `make tf-plan` shows no unexpected changes; `make test-tf-run` passes.
- `tailscale status` (on a client of your tailnet) lists the exit nodes; pick one and confirm
  your egress IP changes.
- `git -C vendor/headlamp status` is clean; `git status` in the private repo is clean after a
  successful apply (the state commit was made for you).

### 2.11 Updating headlamp later

```bash
git -C vendor/headlamp fetch origin
git -C vendor/headlamp log --oneline HEAD..origin/main      # read what changes
git -C vendor/headlamp checkout <tag-or-commit>
make tf-plan                                                 # expect no diff unless you intend one
git add vendor/headlamp && git commit -m "chore: bump headlamp to <ref>"
```

## 3. Safety checklist for agents operating on a bootstrapped repo

- Ask before any apply/deploy/destroy; show the plan first.
- Never print, log, or paste secret values (age keys, auth keys, API keys, decrypted state).
- Decrypted material exists only transiently; if a run dies, `terraform/terraform.tfstate`
  may be the only copy of newer state — do not delete it, run `make tf-save`.
- Do not "fix" a failing hook with `--no-verify` without telling the operator why.
- Prefer reversible, narrow actions on live hosts; state what you changed afterwards.

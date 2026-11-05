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
- Versions: strict semver, one version for the repo; see `VERSIONING.md`. Mark a breaking change with `!` or a
  `BREAKING CHANGE:` footer, since releases are checked against commit subjects. Note user-visible changes under
  `Unreleased` in `CHANGELOG.md`. Never move or delete a release tag.
- Manuals: write and update them in Simplified Technical English, to `docs/writing-manuals.md`. CI checks each manual a
  PR changes (`make check-manuals`). Accuracy comes first: keep every value, limit, condition and warning.
- A new or renamed image goes in `.github/components.json`, and in `docs/images.md`; CI builds from that list.
- Verify before claiming something works: `make test-tf-run`; `shellcheck` on shell scripts;
  `terraform fmt -check && terraform validate` for module changes; `ansible-playbook
  --syntax-check` for roles; `docker build` for app images. Say what you ran and what you did not.

### Repo map

| Path | What it is |
|---|---|
| `.github/components.json`, `.github/workflows/`, `VERSIONING.md`, `docs/images.md` | images CI publishes to GHCR, rebuilt per component; the release rules and their checks (`scripts/check-release.sh`); what each image is and how they fit together |
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

## 1a. Agentic development workflow

Adapted from the process of `valentinalexeev/agentdev` (its `CLAUDE.md`, sections 0 to 5). The rules in section 1
(public repo, no geographic references, denylist) apply on top of everything here and win on conflict.

### Starting a session

1. Read this file.
2. Load the bot identity: `source ~/.config/github-bot/env.sh`, then check `gh api user -q .login` prints
   `valentinalexeev-bot`.
3. Open GitHub issues of `valentinalexeev/headlamp` **assigned to `valentinalexeev-bot`** only, in this order:
   open `incident` issues, then `in-progress`, then the rest by priority (lowest number first).
4. Work the issue, check it against its acceptance criterion, update the issue.

### Status lives in GitHub issues only

- Every piece of unfinished work is an issue: a planned change, a leftover found in a session, an incident.
  "To do later" in code, docs or commit messages without an issue link is not allowed.
- Labels: `AI` (created by an agent), `in-progress`, `needs-decision` (waits for the owner), `blocked`,
  `roadmap` (umbrella issue with the status comment), `deferred` (reason in a comment), `incident`, `ADR`, plus the
  existing topic labels. Add a new label only if none fits.
- Statuses: not started = open without `in-progress`; in progress = `in-progress`; done = closed as completed **only**
  when the acceptance criterion is met, with a comment (what was done, checks, PR); partial = stays open with a list of
  what remains; rejected = closed as not planned with a reason.
- Commits and PRs reference the issue (`#NN`): `Closes #NN` only when the acceptance criterion is met, else `Refs #NN`.
- **Every question to the owner goes on an issue** (form below), even when also asked in chat, so the owner sees all
  open decisions in `label:needs-decision` without reading the chat.
- Issue text is public: run `scripts/check-denylist.sh --text FILE` on it before posting (section 1).

### Bot and owner

| Action | Bot `valentinalexeev-bot` | Owner `valentinalexeev` |
|---|---|---|
| Issues, comments, labels | yes, marked (below) | yes |
| Decide an open question | **no**, a bot comment is never a decision | yes, by comment |
| Branch, commit, push to a branch | yes, always as the bot | yes |
| Act as the owner (owner git identity, credentials, `gh` login) | **no**, unless the owner gave explicit permission **in that ticket, for that change** | yes |
| Push to `develop` or `main` | **no** | via PR |
| Open / update PR, fix mechanical conflicts | yes | yes |
| Merge a PR | only after an owner **Approve** review on the PR's current head commit and green CI | yes |
| Repo settings, secrets, branch protection, release tags | **no** | yes |

- **Marking.** Issues the bot creates get the `AI` label. Bodies, comments and PR descriptions start with
  `🤖 AI agent:`. Commits are authored by the bot; add
  `Co-authored-by: Valentin Alexeev <valentinalexeev@users.noreply.github.com>` when the owner requested the change.
- **Queue = assignee.** The agent works only on issues the owner assigned to `valentinalexeev-bot` and never assigns
  itself. When it needs a decision, or is done and waits for review or merge, it reassigns the issue to
  `valentinalexeev` with a comment saying what is needed.
- **Questions.** A human decision → comment + `needs-decision`: numbered points with options, a link to the exact
  section, a default proposal with reasons ("as proposed" counts as the answer), and whether it blocks. Purely
  technical choices are decided by the agent in an ADR (`docs/adr/`) without asking. Ask before the work, not at the end.
- **Recording answers.** On an owner answer: comment "Decision recorded: …" with a link, move it into the doc or ADR,
  drop `needs-decision`, continue if unblocked. An ambiguous answer → ask again, don't guess.
- **Never as the owner.** When the bot cannot do something itself (e.g. its token lacks `workflow` scope for
  `.github/workflows/`), it posts the exact diff on the ticket and waits. If the bot token fails (401), tell the owner
  and stop; never fall back to the owner account.

### Branches and pull requests

- Branches start from `develop` and PRs target `develop`; `develop` reaches `main` only through a release PR
  (`VERSIONING.md`). Names: `<prefix>/<issue>-<slug>`, lower-case, prefixes `feat/`, `fix/`, `docs/`, `refactor/`,
  `test/`, `ci/`, `chore/`, `hotfix/` (incidents), `release/X.Y.Z`. Example: `fix/97-sidecar-retry`.
- Commits and PR titles: Conventional Commits (section 1, checked by `pr-title`), footers `Refs #NN` / `Closes #NN`.
  Commit and push as you go.
- PR description (template in `.github/pull_request_template.md`): `🤖 AI agent:`, issue link, what was done,
  **checks run with results**, what remains open (as issue links), files shared with other open PRs.
- Request the owner's review on opening and after every push that makes the last approval stale:
  `gh api -X POST repos/valentinalexeev/headlamp/pulls/NN/requested_reviewers -f "reviewers[]=valentinalexeev"`.
- Before proposing a merge: CI green, and an independent review pass (a separate agent or reviewer) with its result in
  the PR. Blocking findings are fixed first.
- **Documentation is part of the change:** a PR that changes behaviour updates the affected README/manuals and adds an
  `Unreleased` line to `CHANGELOG.md` in the same PR.
- **Merge permission is a review with state `APPROVED` by `valentinalexeev` on the current head commit.** Comments,
  chat messages and others' approvals are not permission. Check
  `gh api repos/valentinalexeev/headlamp/pulls/NN/reviews` right before merging, then merge with the head `sha`
  pinned, as a merge commit. After a merge, check other open PRs for conflicts; merge `develop` into them when the
  conflict is mechanical, ask the owner when it is semantic.

### Releases, tags and images

The rules and their checks are in [`VERSIONING.md`](VERSIONING.md); this is the agent's view of them.

- **Branches.** `develop` is the integration branch: every feature, fix and docs PR targets it. `main` holds only
  released (or about to be released) code and receives PRs from `develop` (a release) or from `hotfix/` branches.
  Images publish from `main` and tags only, never from `develop` or feature branches (PRs build and test, no push).
- **Version.** Strict SemVer, one version for the whole repo in `VERSION`. Before 1.0.0 a breaking change moves MINOR.
  The bump is derived from commit subjects since the last stable tag: `feat` → at least MINOR, `!` or
  `BREAKING CHANGE:` → MAJOR (MINOR before 1.0.0), the rest PATCH. So a wrong commit type is a release bug: pick it
  carefully, and check `scripts/check-conventional.sh "<title>"` on PR titles.
- **Changelog.** Every user-visible change adds a line under `## Unreleased` in `CHANGELOG.md` (Keep a Changelog
  sections), breaking ones marked **BREAKING** with the migration step.
- **Cutting a release** (the bot may prepare it, the owner merges and tags):
  1. Branch `release/X.Y.Z` from `develop`; commit `chore(release): vX.Y.Z` that sets `VERSION` and moves `Unreleased`
     under `## X.Y.Z - <date>`. PR into `develop`, owner approves and merges.
  2. PR `develop` → `main` titled `chore(release): vX.Y.Z`. The owner approves and merges (merge commit).
  3. The owner, or the bot only with explicit permission in that release's issue or PR, tags the merge commit on `main`:
     `git tag vX.Y.Z <commit> && scripts/check-release.sh vX.Y.Z && git push origin vX.Y.Z`.
     Release candidates use `vX.Y.Z-rc.N`.
  4. CI (`images.yml`) runs `check-release.sh` as a gate: tag format, `VERSION` match, commit on `main`, next version
     with no skipping, bump at least what the commits need, milestone `X.Y.Z` (if any) has no open issues. Then it
     tags the already-built images and creates the GitHub release. Check the run and report its result on the issue.
- **Hotfix.** `hotfix/NN-…` from `main`, PR into `main` with a PATCH bump in the same PR, tag as above, then merge
  `main` back into `develop` through a PR. There is no fix release to an older line.
- **Tags are immutable.** Never move or delete a release tag or re-publish a version; fix a bad release with a new one.
- **Images.** `ghcr.io/valentinalexeev/headlamp/<image>`, one per entry in `.github/components.json` (new or renamed
  image: add it there and to `docs/images.md`). Only a component whose source changed is rebuilt (`src-<hash>`); a
  release reuses the images `main` built, so what was tested ships. Tags: `src-<hash>` and `sha-<commit>` (fixed),
  `main` (moves), `X.Y.Z` / `X.Y.Z-rc.N` (fixed), `X.Y`, `X`, `latest` (move; `X`/`latest` from 1.0.0). Deployments
  pin `X.Y.Z` or a digest, never `main` or `latest`. A forced full rebuild is `workflow_dispatch` with `rebuild_all`
  and is an owner action.

### Commits, changelog, manuals and the wiki

- **Conventional Commits 1.0.0** for every commit and PR title: `<type>[(scope)][!]: <description>`, imperative,
  lower-case, no trailing period, then an optional body and footers. Types: `feat`, `fix`, `docs`, `style`,
  `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`. Scope is the component (`node-control`,
  `funnel-ingress`, `headscale`, `exit-ams`, `tailguard`, `task-runner`, `route-provisioner`, `terraform`, `ansible`,
  `ci`, ...). A breaking change uses `!` **and** a `BREAKING CHANGE: <what and how to migrate>` footer. Footers also
  carry `Refs #NN` / `Closes #NN` and `Co-authored-by:`. PRs merge with a merge commit, so every commit on the branch
  reaches `develop`: each one must pass `scripts/check-conventional.sh` and the denylist, not only the title.
- **Changelog** ([Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)): every PR with a user-visible change
  adds a line under `## Unreleased` in `CHANGELOG.md`, in the section that fits (Added, Changed, Deprecated, Removed,
  Fixed, Security). Write it for an operator who upgrades: what changed and what they must do. Breaking lines start with
  **BREAKING** and give the migration step. Refactors, tests and CI-only changes need no line. A release moves
  `Unreleased` under `## X.Y.Z - YYYY-MM-DD`; nothing else edits released sections.
- **Manuals in ASD-STE100 Simplified Technical English**, to [`docs/writing-manuals.md`](docs/writing-manuals.md),
  which also says which files are manuals. Procedures are numbered steps with one instruction each, a WARNING or
  CAUTION comes before the step it is about, sentences stay short, and words come from the approved list plus this
  project's technical names. CI checks changed manuals (`make check-manuals`, workflow `manuals`); run it before
  pushing. Accuracy comes first: keep every value, limit and warning when rewriting. The independent reviewer checks STE
  on every PR that changes a manual. Agent rules (this section, `AGENTS.md`), the changelog and code comments use
  normal technical English.
- **Wiki.** The documentation is published to the GitHub wiki by the `wiki` workflow on every push to `main` (so on
  release, not on each merge to `develop`): `README.md` → Home, `docs/<name>.md` → `<name>`, plus `VERSIONING.md` and
  `CHANGELOG.md` (`scripts/wiki-render.sh`). The repository is the source: never edit the wiki by hand, its next sync
  overwrites it. Wiki pages are public too, so the rules of section 1 apply to everything in `docs/`. To preview:
  `scripts/wiki-render.sh "$(mktemp -d)" valentinalexeev/headlamp`.

### Incidents

Something that already went wrong: red CI on `develop`/`main`, a leaked or expired secret, a denylist hit in history,
an agent that damaged work. Open an issue at once (labels `incident`, `AI`, template *Incident*): what happened, when
and how found, impact, immediate actions. Never paste secrets or denylisted text, even partially. Fix in
`hotfix/NN-…` with `Closes #NN`; the closing comment states root cause, fix and what prevents a repeat. Revoking
secrets and rewriting history are owner actions (section 1).

## 2. Bootstrap your own infra

At the end you have a private repo `<your>-infra`. It contains your config and encrypted state,
with this repo at `vendor/headlamp`. Do the steps in order. Stop to confirm before anything
that costs money or touches a real account.

### 2.0 Prerequisites

- Accounts: Fly.io (required, for headscale). If you want VM exit nodes, get at least one VM provider
  (Yandex Cloud, Oracle Cloud or AWS). A domain that you control is recommended
  (MagicDNS base domain, stable node names).
- Tools: `git`, `sops`, `age`, `terraform` (>= 1.5), `ansible` (+ collections, below), `flyctl`,
  `jq`, `python3`, `shellcheck`. Add the provider CLIs that you use (`yc`, `oci`, `aws`). Optional:
  1Password CLI `op`. Without it, supply `SOPS_AGE_KEY` from a file or keychain (see 2.3).
- Dedicated cloud credentials with narrow scope for each provider (a separate CLI profile such as
  `~/.oci/config [headlamp]`). Never use your everyday identity.

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

Rules for the submodule: never edit files under `vendor/headlamp` as part of site work.
Make a change to headlamp in a clone of the public repo. Push it there. Then pull it in by bumping
the pointer. Anyone who clones your private repo uses `git clone --recurse-submodules`
(or `git submodule update --init`).

### 2.2 Private repo layout

Start from `vendor/headlamp/examples/private-repo/`. Copy it, then edit it. It matches this layout.

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

`.sops.yaml`: every rule must list the **same** recipients (`tf-run.sh` encrypts to one set):

```yaml
creation_rules:
  - path_regex: .*\.(sops|enc)\.(yaml|json)$
    age: age1<main-public-key>,age1<backup-public-key>
```

Make the private key available as `SOPS_AGE_KEY` (env var that holds the key text). With 1Password, put
`export SOPS_AGE_KEY := $(shell op read "op://<vault>/<item>/<field>")` in the Makefile (make fetches it
once for each `make` run). Without 1Password, put `export SOPS_AGE_KEY := $(shell cat ~/age-main.txt)` in the Makefile.

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

To create an encrypted secrets file:

1. Write the plaintext to a temp path **outside the repo**.
2. Run `sops -e --filename-override secrets/<name>.sops.yaml <tmp> > secrets/<name>.sops.yaml`.
   `--filename-override` makes the creation rules match the destination name, not the temp path.
3. Delete the temp file.

The hook refuses plaintext secret files and unencrypted `*.enc.*` and `*.sops.*` files.

### 2.4 headscale control server (Fly.io)

1. Copy `vendor/headlamp/apps/fly/headscale/{fly.toml,headscale.yaml,acl.json}` into
   `fly/headscale/`. Edit the Fly app name, `server_url` and `base_domain`. `base_domain` must differ from
   the domain of `server_url`. Edit the ACL (`tagOwners`, `autoApprovers.exitNode`) for your users and tags.
2. Create the app and its persistent volume:
   `flyctl apps create <app>` and `flyctl volumes create headscale_data -a <app> -r <region> -n 1`.
   Then deploy with the overlay procedure (2.5).
3. Create a user and pre-auth keys on the server
   (`flyctl ssh console -a <app> -C "headscale users create <user>"`, then
   `headscale preauthkeys create …`). The flags differ between headscale versions, so check `--help`.
   Use reusable, non-ephemeral keys for exit nodes. Keep keys only in sops-encrypted files or
   `flyctl secrets`.

### 2.5 Deploying Fly apps from the submodule (overlay procedure)

The Fly Dockerfiles `COPY` some site files from the app directory (`headscale.yaml`, `acl.json`,
`direct-hosts.txt`, `app/nodes.json`, `fly.toml`). The build context needs your files, and the
submodule must stay pristine. `examples/private-repo/fly/deploy.sh` does exactly that:

- It refuses to run if `vendor/headlamp` is dirty.
- It copies `fly/<app>/` over `vendor/headlamp/apps/fly/<app>/`.
- It runs `flyctl deploy` there.
- It restores the submodule on exit (even on failure).

```bash
fly/deploy.sh headscale
fly/deploy.sh exit-ams --build-arg TSVERSION=<x.y.z>
fly/deploy.sh headscale --build-arg HEADSCALE_VERSION=<tag>   # default is pinned in the Dockerfile
```

The headscale image tag is pinned on purpose. `jauderho/headscale:latest` stopped shipping a shell in
v0.29.5, and this breaks the `apk add` step of the Dockerfile. Before you bump the tag, check that the new tag has `/bin/sh`.

Deploying is a real change, so confirm with the operator first. Afterwards, `git -C vendor/headlamp status` must
be clean.

### 2.6 VM exit nodes (Terraform) and state

In `terraform/` of the private repo, write a root stack that uses the modules by path:

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

Add also `providers.tf` (dedicated profiles, no inline credentials), `versions.tf`, `variables.tf`,
and secrets in `terraform/secrets.auto.tfvars.enc.json` (sops-encrypted JSON of tfvars).
Each module documents its inputs in `variables.tf`. The Yandex module also
takes `folder_id`, `zone`, `subnet_id`, `image_id`, sizing, `user_data` and `extra_metadata`.

**State is a sops-encrypted file in git** (`terraform/terraform.tfstate.enc.json`). `tf-run.sh` handles it.
There is no remote backend. Always use `make tf-plan` and `make tf-apply`. Never use bare
`terraform` in that directory.

For what the wrapper guarantees and how to recover, see
`README.md` → "Terraform state workflow". In short:

- Apply refuses to run from a dirty or stale checkout.
- The wrapper verifies, commits and pushes the state after every run that changed it (even a failed run).
- The wrapper never deletes plaintext state on error. The next run refuses until you run `make tf-save`.

The first run with no state works: apply creates and commits the state.

Keep one operator machine and
a private remote. You cannot resolve merge conflicts in the encrypted file by hand. Take one
side and plan again.

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
- `inventory/hosts.yml`: use the group `exit_nodes_vm`. For nodes that get route
  lists, use also the group `route_provisioned`. Give each host `ansible_host` (stable MagicDNS name) and `ansible_user`.
- `site.yml`: apply role `tailscale` to `exit_nodes_vm`. Apply `route-provisioner` to
  `route_provisioned`. Copy the structure of `vendor/headlamp/ansible/site.yml`. Remove the
  node-control rendering play, unless you use node-control.
- `group_vars/all/main.yml`: `headscale_login_server`, `tailscale_version`.
  `group_vars/all/vault.sops.yaml` (encrypted): per-host auth keys.
- `host_vars/<host>.yml`: `tailscale_hostname`, `tailscale_authkey`, optional
  `tailscale_advertise_exit_node`, and the route lists `route_asns`, `route_hosts` and
  `route_hosts_direct`. See `ansible/roles/route-provisioner/defaults/main.yml` for what each does
  and when to prefer `route_hosts_direct`. Do NOT put large shared ASNs like AWS or Akamai in
  `route_asns`.
- The role publishes hosts in `route_hosts` and `route_hosts_direct` only if this node can open a TCP
  connection to them. `route_verify_reachability` controls this (default on). The ports, timeout and attempts are `route_probe_*`.
  A service that filters the address range of the node is therefore not advertised. Test: `bash scripts/test/route-provisioner/run.sh`.
- Run from `ansible/` with `SOPS_AGE_KEY` set:
  `ansible-playbook site.yml --extra-vars tailscale_version=<x.y.z>`. Add `--check --diff` first.

### 2.8 Fly exit node and node-control (optional)

- `apps/fly/exit-ams`: copy `fly.toml` and `direct-hosts.txt` to `fly/exit-ams/`. Set your app name.
  Run `flyctl volumes create tailscale_state …`. Run `flyctl secrets set TAILSCALE_AUTH_KEY=… HEADSCALE_LOGIN_SERVER=…`.
  Deploy with 2.5. Its `direct-hosts.txt` is a list of hostnames. The node resolves them to `/32` routes
  and advertises these routes. Use your own list, not the example.
- `apps/fly/node-control` is a Go service (one binary, embedded tailscale). It has a start/stop UI and API for your exit nodes.
  It is reachable only over the tailnet by design (no public service). It discovers exit nodes from headscale.
  It lists a node with `tag:exit`. A `tag:cloud-<provider>` tag (`yandex`, `oci`, `selectel`, `fly`) plus the reported name of the node
  selects the VM or app to control.

  The optional JSON `app/nodes.json` (see `testdata/nodes.example.json`) only adds overrides: explicit connection
  blocks, labels, aliases and `hidden`. Keep the real file in
  your private repo and lay it over the app dir with the overlay procedure.

  Provider credentials are Fly secrets (`YANDEX_SA_KEY_JSON`, `FLY_API_TOKEN`, the
  OCI keys, `SELECTEL_*`, `HEADSCALE_API_KEY`). Keep the tailnet identity on the `tailscale_state` volume (see its
  `fly.toml`), or in a shared Redis with `REDIS_URL` and an encryption key. Details: `apps/fly/node-control/README.md`.
- Tasks module (optional, needs that Redis): scheduled browser-script checks that run on a node, with retries, alerts,
  metrics and a heartbeat. To set it up, do these steps:
  1. Set `REDIS_URL`, `NODE_CONTROL_STATE_KEY`, `TASKS_RUNNER_TOKEN`, `TASKS_ADMIN_USERS` and
     `ENROLL_ALLOWED_HOSTS`. The module requires them.
  2. Deploy the `task-runner` role (`ansible/roles/task-runner`) to the nodes that must run scripts.
  3. Give those nodes the runner tag (`TASKS_RUNNER_TAG`, default `tag:task-runner`) as an admin (`scripts/headscale-tag-node.sh`).
     node-control never uses untagged nodes. A `node` target means the headscale given name.
  4. In the `tagOwners` of the policy, list only the admin or service identity as owner of that tag.
  5. In the tailnet ACL, allow node-control to reach the runner port of that tag.
  6. Manage config, scripts and secrets from your private repo with `scripts/tasks.sh`
     (example files: `examples/private-repo/fly/node-control/tasks/`).

  **WARNING:** `tagOwners` is the trust boundary. It decides who can have the tag at registration. On headscale before 0.28,
  a node can also request the tag later. On 0.28+, tagging a user-owned node drops its user ownership.

  For variables, API, metrics and the trust model, see
  `apps/fly/node-control/README.md`, section "Tasks module".

### 2.9 tailguard (optional, operator-run)

Use this for a Docker host that must offer a WireGuard server routed through an exit node. Do these steps:

1. Copy `vendor/headlamp/apps/tailguard/` to the host.
2. Create `.env` from `.env.example`.
3. Create `config/wg0.conf` from the example.
4. Run `docker compose up -d`.

`apps/tailguard/README.md` documents it end to end (key generation, router client config, verification script, the
watchdog and exit-node-tracker sidecars). Its state (`.env`, `config/wg0.conf`, `state/`) lives
on the host only and is gitignored. Keep it out of both repos.

### 2.10 Verify

- `make tf-plan` shows no unexpected changes, and `make test-tf-run` passes.
- `tailscale status` (on a client of your tailnet) lists the exit nodes. Pick one and confirm
  that your egress IP changes.
- `git -C vendor/headlamp status` is clean. `git status` in the private repo is clean after a
  successful apply, because the wrapper made the state commit for you.

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

# examples/private-repo

A starting point for YOUR private site repo. The private repo vendors this repo as a submodule at
`vendor/headlamp`. Copy the contents of this directory to the root of your private repo. Then follow
`CLAUDE.md` section 2 step by step.

Everything here is a placeholder. Replace the `<...>` values,
`example.com`, and the sample node names with your own. The core repo does not use anything in this directory.

```
Makefile                      only your values (apps, node-control URL, sops key), then include vendor/headlamp/mk/site.mk
.sops.yaml.example            age recipients (rename to .sops.yaml)
.gitignore                    keeps plaintext state/secrets out of your repo
terraform/                    root stack using vendor/headlamp/terraform/modules (+ example vars)
ansible/                      ansible.cfg, site.yml, inventory + vars examples
fly/deploy.sh                 deprecated wrapper for old scripts: use make deploy-<app>
fly/headscale/fly.toml        example headscale overlay (checked by make release-preflight)
fly/node-control/tasks/       placeholder task config and a commented task script (tasks module)
```

`fly/node-control/tasks/` is only an example. It shows how to keep your scheduled-task config and scripts in git.
Push them with `scripts/tasks.sh` (see "Tasks module" in `apps/node-control/README.md`).
node-control never reads git. The node-control image does not copy this directory, so the overlay deploy ships nothing from it.
Rename `tasks.example.json` to `tasks.json`. Rename the script to the name that the `script` field of your task uses.

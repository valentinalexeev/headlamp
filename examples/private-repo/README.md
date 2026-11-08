# examples/private-repo

A starting point for YOUR private site repo. The private repo vendors this repo as a submodule at
`vendor/headlamp`. Copy the contents of this directory to the root of your private repo. Then follow
`CLAUDE.md` section 2 step by step.

Everything here is a placeholder. Replace the `<...>` values,
`example.com`, and the sample node names with your own. The core repo does not use anything in this directory.

```
Makefile                      glue: sops key, tf-run.sh targets, ansible, version bump
.sops.yaml.example            age recipients (rename to .sops.yaml)
.gitignore                    keeps plaintext state/secrets out of your repo
terraform/                    root stack using vendor/headlamp/terraform/modules (+ example vars)
ansible/                      ansible.cfg, site.yml, inventory + vars examples
fly/deploy.sh                 overlay deploy for the Fly apps
fly/headscale/fly.toml        example headscale overlay (checked by make release-preflight)
fly/node-control/tasks/       placeholder task config and a commented task script (tasks module)
```

`fly/node-control/tasks/` is only an example. It shows how to keep your scheduled-task config and scripts in git.
Push them with `scripts/tasks.sh` (see "Tasks module" in `apps/node-control/README.md`).
node-control never reads git. The node-control image does not copy this directory, so the overlay deploy ships nothing from it.
Rename `tasks.example.json` to `tasks.json`. Rename the script to the name that the `script` field of your task uses.

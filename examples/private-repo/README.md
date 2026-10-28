# examples/private-repo

A starting point for YOUR private site repo, which vendors this repo as a submodule at
`vendor/headlamp`. Copy this directory's contents to the root of your private repo, then follow
`CLAUDE.md` section 2 step by step. Everything here is a placeholder: replace the `<...>` values,
`example.com`, and the sample node names with your own. Nothing in this directory is used by the
core repo itself.

```
Makefile                      glue: sops key, tf-run.sh targets, ansible, version bump
.sops.yaml.example            age recipients (rename to .sops.yaml)
.gitignore                    keeps plaintext state/secrets out of your repo
terraform/                    root stack using vendor/headlamp/terraform/modules (+ example vars)
ansible/                      ansible.cfg, site.yml, inventory + vars examples
fly/deploy.sh                 overlay deploy for the Fly apps
fly/node-control/tasks/       placeholder task config and a commented task script (tasks module)
```

`fly/node-control/tasks/` is only an example of how to keep your scheduled-task config and scripts in git: push
them with `scripts/tasks.sh` (see "Tasks module" in `apps/fly/node-control/README.md`). node-control never reads git,
and the node-control image does not copy this directory, so the overlay deploy ships nothing from it. Rename
`tasks.example.json` to `tasks.json` and the script to the name your task's `script` field uses.

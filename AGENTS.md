# AGENTS.md

All instructions for agents and operators working in this repository live in
**[CLAUDE.md](CLAUDE.md)** — read it before making any change. It covers the repo rules, the
public-repo / private-site-repo split, and the step-by-step guide to bootstrapping your own
infrastructure with a private repo that vendors this one as a git submodule.

The non-negotiables, in case you read nothing else:

- This repo is meant to be public: never commit secrets, state, real domains/IDs, or personal
  route lists. Use placeholders.
- Do not apply, deploy, destroy, or push unless the operator asked for exactly that.
- Never print secret values; never delete a leftover plaintext `terraform.tfstate` (run
  `make tf-save`).

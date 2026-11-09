# Makefile for a private site repo that vendors headlamp at vendor/headlamp (#194). The targets come from headlamp:
# this file holds only the values of YOUR site. `make help` lists the targets.
#
# The age private key is fetched ONCE per `make` invocation and exported for sops/terraform helpers.
# With 1Password:  op read "op://<vault>/<item>/<field>"   Without: cat a key file you keep outside the repo.
export SOPS_AGE_KEY := $(shell op read "op://<vault>/<item>/<field>")

H := vendor/headlamp
# The Fly apps that `make deploy-all` deploys. Each has its overlay files in fly/<app>/.
FLY_APPS := headscale node-control exit-node
# node-control on the tailnet, for `make policy-*` and `make tasks-*`.
NODE_CONTROL_URL := https://<node-control>.<your-tailnet-domain>

include $(H)/mk/site.mk

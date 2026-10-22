# Glue for a private site repo that vendors headlamp at vendor/headlamp.
# The age private key is fetched ONCE per `make` invocation and exported for sops/terraform helpers.
# With 1Password:  op read "op://<vault>/<item>/<field>"   Without: cat a key file you keep outside the repo.
export SOPS_AGE_KEY := $(shell op read "op://<vault>/<item>/<field>")
export TF_DIR := terraform
H := vendor/headlamp

.PHONY: hooks tf-plan tf-apply tf-save ansible-site bump-tailscale-version

hooks:
	@git config core.hooksPath $(H)/.githooks

# Terraform state is a sops-encrypted file in git; tf-run.sh guards it (see its header).
tf-plan: hooks
	$(H)/scripts/tf-run.sh plan $(ARGS)
tf-apply: hooks
	$(H)/scripts/tf-run.sh apply $(ARGS)
tf-save: hooks
	$(H)/scripts/tf-run.sh save

TAILSCALE_VERSION_VALUE = $(shell tr -d '[:space:]' < TAILSCALE_VERSION)

ansible-site:
	cd ansible && ansible-playbook site.yml -i inventory/hosts.yml --extra-vars "tailscale_version=$(TAILSCALE_VERSION_VALUE)"

# Upgrades tailscale on the VM exit nodes (real, live change: restarts tailscaled). The Fly apps
# pick the version up from a build arg at deploy time, see the hint printed at the end.
bump-tailscale-version:
	@test -n "$(VERSION)" || (echo "usage: make bump-tailscale-version VERSION=x.y.z" && exit 1)
	@echo "$(VERSION)" > TAILSCALE_VERSION
	cd ansible && ansible-playbook ../$(H)/ansible/playbooks/bump-tailscale-version.yml -i inventory/hosts.yml --extra-vars "tailscale_version=$(VERSION)"
	@echo "VM exit nodes updated to $(VERSION). Now redeploy the Fly exit node, e.g.:"
	@echo "  fly/deploy.sh exit-ams --build-arg TSVERSION=$(VERSION)"

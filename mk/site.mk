# Site Makefile part, shipped by headlamp (#194). A site repo that vendors headlamp includes it:
#
#   H := vendor/headlamp
#   include $(H)/mk/site.mk
#
# and sets only its own values (below, with ?=). Then a headlamp release that moves a path or adds a build arg needs no
# change in the site repo. `make help` lists the targets. Values with no default are needed only by the targets that
# use them; such a target stops with a clear message when its value is missing.

H                 ?= vendor/headlamp
# Fly apps: the overlay files of app <a> are in $(OVERLAY_DIR)/<a>/; FLY_APPS lists the apps `deploy-all` deploys.
OVERLAY_DIR       ?= fly
FLY_APPS          ?=
# Ansible: the inventory and the playbook, relative to ANSIBLE_DIR.
ANSIBLE_DIR       ?= ansible
INVENTORY         ?= inventory/hosts.yml
SITE_PLAYBOOK     ?= site.yml
# The fleet tailscale pin: one file, read by Ansible (tailscale_version), deploy-fly.sh (TSVERSION) and the checks.
TAILSCALE_VERSION_FILE ?= TAILSCALE_VERSION
# node-control on the tailnet, for policy and tasks (headscale-policy.sh, tasks.sh).
NODE_CONTROL_URL  ?=
POLICY_FILE       ?= $(OVERLAY_DIR)/headscale/acl.json
TASKS_FILE        ?= $(OVERLAY_DIR)/node-control/tasks/tasks.json
TASKS_SCRIPTS     ?= $(OVERLAY_DIR)/node-control/tasks/scripts
TASKS_SECRETS     ?=
# Terraform state helper.
TF_DIR            ?= terraform
export TF_DIR

tsv = $(shell tr -d '[:space:]' < $(TAILSCALE_VERSION_FILE) 2>/dev/null)
need = $(if $($(1)),,$(error $(1) is not set; set it in your Makefile before `include $(H)/mk/site.mk`))

.PHONY: help hooks submodule submodule-check deploy-% plan-deploy-% deploy-all tf-plan tf-apply tf-save ansible-site \
  ansible-check bump-tailscale-version policy-check policy-diff policy-apply tasks-check tasks-diff tasks-push \
  rotate-api-key-check verify

help:
	@echo "headlamp site targets ($(H)/mk/site.mk):"
	@echo "  deploy-<app>             overlay deploy of one Fly app (plan-deploy-<app>: show the command only)"
	@echo "                           headscale gets a volume snapshot first; NO_SNAPSHOT=1 skips it"
	@echo "  deploy-all               deploy every app in FLY_APPS ($(FLY_APPS))"
	@echo "  tf-plan tf-apply tf-save terraform through tf-run.sh (encrypted state)"
	@echo "  ansible-site             run $(SITE_PLAYBOOK) with the fleet tailscale pin ($(tsv)); skips nodes that are down"
	@echo "  ansible-check            the same in --check --diff mode"
	@echo "  bump-tailscale-version VERSION=x.y.z   write the pin, update the VM nodes, deploy the Fly exit node"
	@echo "                           EXCLUDE=n1,n2 skips nodes; ONLY_PENDING=1 retries the pending ones"
	@echo "  policy-check policy-diff policy-apply  the headscale ACL through node-control"
	@echo "  tasks-check tasks-diff tasks-push      node-control scheduled tasks"
	@echo "  rotate-api-key-check     check when the headscale API key expires"
	@echo "  submodule-check          refuse when the $(H) pin and its checkout differ"
	@echo "  verify                   the read-only checks above, in one run"

hooks:
	@git config core.hooksPath $(H)/.githooks

# The submodule pin in the index must be the commit checked out in $(H). A plain `git submodule update --init` would
# reset a pin bump that is not committed yet, so this target never runs it when the two differ.
submodule-check:
	@pin="$$(git ls-files -s $(H) | awk '{print $$2}')"; head="$$(git -C $(H) rev-parse HEAD 2>/dev/null)"; \
	  if [ -z "$$head" ]; then echo "$(H) is not checked out: run 'make submodule'"; exit 1; fi; \
	  if [ "$$pin" != "$$head" ]; then echo "$(H) is at $$head but the index pins $$pin."; \
	    echo "Commit the new pin (git add $(H)) or check out the pinned commit (git -C $(H) checkout $$pin)."; exit 1; fi; \
	  echo "$(H) is at the pinned commit $$pin"

submodule:
	@if git -C $(H) rev-parse HEAD >/dev/null 2>&1; then $(MAKE) --no-print-directory submodule-check; \
	  else git submodule update --init $(H); fi

# Overlay deploy. Any flyctl args go in ARGS, for example ARGS="--strategy immediate". The apps in SNAPSHOT_APPS
# (default headscale) get a volume snapshot first, and the deploy stops when it fails (#197). NO_SNAPSHOT=1 skips it.
deploy-%: submodule-check
	$(H)/scripts/deploy-fly.sh $* $(ARGS)

plan-deploy-%:
	$(H)/scripts/deploy-fly.sh --dry-run $* $(ARGS)

deploy-all: submodule-check
	$(call need,FLY_APPS)
	@set -e; for a in $(FLY_APPS); do echo "== deploy $$a"; $(H)/scripts/deploy-fly.sh $$a $(ARGS); done

tf-plan: hooks
	$(H)/scripts/tf-run.sh plan $(ARGS)
tf-apply: hooks
	$(H)/scripts/tf-run.sh apply $(ARGS)
tf-save: hooks
	$(H)/scripts/tf-run.sh save

# Fleet runs go through fleet-run.sh (#199): a node that does not answer is not waited for, and the run ends with
# "done:" and "pending: <node> (reason)". EXCLUDE=n1,n2 skips nodes; ONLY_PENDING=1 retries the pending ones.
# PROBE_TIMEOUT (default 15 s) is the wait for each node. Exit 2 means some nodes are pending.
fleet = cd $(ANSIBLE_DIR) && INVENTORY=$(INVENTORY) $(CURDIR)/$(H)/scripts/fleet-run.sh

ansible-site:
	$(fleet) $(SITE_PLAYBOOK) --extra-vars "tailscale_version=$(tsv)" $(ARGS)

ansible-check:
	cd $(ANSIBLE_DIR) && ansible-playbook $(SITE_PLAYBOOK) -i $(INVENTORY) --extra-vars "tailscale_version=$(tsv)" --check --diff $(ARGS)

# One pin for the fleet: the VM nodes get it through Ansible, and the Fly exit node through deploy-fly.sh (TSVERSION).
# FLY_EXIT_NODE=0 skips the Fly deploy (for a site with no Fly exit node).
FLY_EXIT_NODE ?= 1
bump-tailscale-version:
	@test -n "$(VERSION)" || { echo "usage: make bump-tailscale-version VERSION=x.y.z"; exit 1; }
	@echo "$(VERSION)" > $(TAILSCALE_VERSION_FILE)
	@$(fleet) $(CURDIR)/$(H)/ansible/playbooks/bump-tailscale-version.yml --extra-vars "tailscale_version=$(VERSION)" $(ARGS); \
	  rc=$$?; [ $$rc = 0 ] || [ $$rc = 2 ] || exit $$rc; [ $$rc = 0 ] || echo "Some VM nodes are pending (above). The Fly exit node is next."
	@if [ "$(FLY_EXIT_NODE)" = 1 ] && [ -d $(OVERLAY_DIR)/exit-node ]; then $(H)/scripts/deploy-fly.sh exit-node; \
	  else echo "no Fly exit node deployed (FLY_EXIT_NODE=$(FLY_EXIT_NODE), overlay $(OVERLAY_DIR)/exit-node)"; fi
	@echo "tailscale $(VERSION): commit $(TAILSCALE_VERSION_FILE)"

policy-check policy-diff policy-apply:
	$(call need,NODE_CONTROL_URL)
	NODE_CONTROL_URL=$(NODE_CONTROL_URL) $(H)/scripts/headscale-policy.sh $(@:policy-%=%) --file $(POLICY_FILE) $(ARGS)

tasks-check tasks-diff:
	$(call need,NODE_CONTROL_URL)
	NODE_CONTROL_URL=$(NODE_CONTROL_URL) $(H)/scripts/tasks.sh $(@:tasks-%=%) --file $(TASKS_FILE) --scripts $(TASKS_SCRIPTS) $(ARGS)

tasks-push:
	$(call need,NODE_CONTROL_URL)
	NODE_CONTROL_URL=$(NODE_CONTROL_URL) $(H)/scripts/tasks.sh push --file $(TASKS_FILE) --scripts $(TASKS_SCRIPTS) \
	  $(if $(TASKS_SECRETS),--secrets $(TASKS_SECRETS)) $(ARGS)

# rotate-headscale-api-key.sh reads HS_APP, STAGE_APPS and DEPLOY_CMD from the environment (see its header).
rotate-api-key-check:
	$(H)/scripts/rotate-headscale-api-key.sh --check

# Read-only: the pin, the plan of each Fly deploy, and (with NODE_CONTROL_URL) the live policy and tasks.
verify: submodule-check
	@test -n "$(tsv)" && echo "tailscale pin: $(tsv)" || { echo "no $(TAILSCALE_VERSION_FILE)"; exit 1; }
	@set -e; for a in $(FLY_APPS); do $(H)/scripts/deploy-fly.sh --dry-run $$a; done
	@if [ -n "$(NODE_CONTROL_URL)" ]; then $(MAKE) --no-print-directory policy-diff tasks-diff; \
	  else echo "NODE_CONTROL_URL is not set: policy and tasks not checked"; fi

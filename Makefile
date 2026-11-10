# Developer targets for the headlamp core repo. Site operations (terraform, ansible, deploys) live in
# YOUR private repo: see examples/private-repo/ and CLAUDE.md.
.PHONY: test-fleet-run test-fly-snapshot test-site-mk release-preflight check-feature-defaults test-feature-defaults hooks test-tf-run test-check-denylist test-exit-node-state test-headscale-policy test-headscale-policy-e2e test-bridge-policy-e2e test-policy-apply-e2e test-serve-e2e test-funnel-tls-e2e test-hysteria-e2e test-tailnet-access-e2e test-oidc-e2e test-ingress-agent test-ansible-roles test-ansible-os check-tailscale-pins test-tailscale-pins check-headscale-pin test-headscale-pin test-route-approval test-compose-control test-control-host-aws test-runtime-config lint test-ssm-state-e2e test-ssm-state-vm-e2e \
  test-tasks-cli test-task-runner test-node-control test-headscale-tag-node test-release test-manual-style check-manuals

hooks:
	@git config core.hooksPath .githooks

test-tf-run:
	./scripts/test/tf-run/run_tests.sh

test-check-denylist:
	./scripts/test/check-denylist/run_tests.sh

test-exit-node-state:
	./scripts/test/exit-node-state/run_tests.sh

# needs docker: the images read site config mounted at runtime (#97). IMAGE=headscale|exit-node|all
test-runtime-config:
	./scripts/test/runtime-config/run_tests.sh $(or $(IMAGE),all)

test-headscale-policy:
	sh scripts/test/headscale-policy/run.sh

test-headscale-tag-node:
	sh scripts/test/headscale-tag-node/run.sh

test-tasks-cli:
	sh scripts/test/tasks/run.sh

test-ingress-agent:
	cd apps/ingress-agent && go vet ./... && go test -race -count=1 ./...

# Role checks that run real ansible against the real tree (#144). Every role test script under tests/ runs.
test-ansible-roles:
	@set -e; for t in ansible/roles/*/tests/*.sh; do echo "== $$t"; bash "$$t"; done

# OS matrix (#170): every role in check mode, then a real apply, in a throwaway systemd container for one target image.
# IMAGE=ubuntu-minimal|ubuntu-server|debian13 (default: all three). ROLES limits the roles; APPLY limits the real apply.
ANSIBLE_OS_IMAGES ?= ubuntu-minimal ubuntu-server debian13
test-ansible-os:
	@set -e; for i in $(or $(IMAGE),$(ANSIBLE_OS_IMAGES)); do echo "== $$i"; APPLY="$(APPLY)" scripts/test/ansible-os/run.sh $$i $(ROLES); done

check-tailscale-pins:
	scripts/check-tailscale-pins.sh

test-tailscale-pins:
	scripts/test/tailscale-pins/run_tests.sh

check-headscale-pin:
	scripts/check-headscale-pin.sh

test-headscale-pin:
	scripts/test/headscale-pin/run_tests.sh

# Route approval by policy (autoApprovers) with real tailscale clients on the pinned headscale; needs docker.
# FROM=<image> first builds the state on that image, then upgrades in place to the pinned one.
test-route-approval:
	sh scripts/test/route-approval/e2e-docker.sh $(if $(FROM),$(FROM) ghcr.io/juanfont/headscale:$$(tr -d '[:space:]' < HEADSCALE_VERSION))

test-task-runner:
	cd apps/task-runner && go vet ./... && go test -race -count=1 ./...

test-release:
	./scripts/test/release/run_tests.sh

test-manual-style:
	./scripts/test/manual-style/run_tests.sh

# Checks every manual, or only those changed since a ref: make check-manuals BASE=origin/main
check-manuals:
	@./scripts/list-manuals.sh $(BASE) | xargs ./scripts/check-manual-style.py

test-node-control:
	cd apps/node-control && go vet ./... && go test -race -count=1 ./...

# on demand (#259): the bridge rules of the example policy on real headscale and tailscale, with and without
# autogroup:internet. Needs docker and jq.
test-bridge-policy-e2e:
	sh scripts/test/bridge-policy/e2e-docker.sh

# on demand (#261): examples/compose/control/scripts/policy-apply.sh with a real headscale in compose. Needs docker.
test-policy-apply-e2e:
	sh scripts/test/policy-apply/e2e-docker.sh

# on demand: needs docker and a headscale image (see the script)
test-headscale-policy-e2e:
	sh scripts/test/headscale-policy/e2e-docker.sh

# On demand: serve (#117) against a real headscale in docker (split DNS to node-control, HTTPS through sidecars).
test-serve-e2e:
	sh scripts/test/serve/e2e-docker.sh

# On demand (#127): funnel on-demand certificates, the real edge with a test ACME server (Pebble) and funnel-ingress.
test-funnel-tls-e2e:
	sh scripts/test/funnel-tls/e2e-docker.sh

# On demand (#110): Hysteria 2 beside VLESS Reality. Tailnet, exit node, bridge, enrolment and real clients in docker.
test-hysteria-e2e:
	sh scripts/test/hysteria/e2e-docker.sh

test-tailnet-access-e2e:
	sh scripts/test/tailnet-access/e2e-docker.sh
	sh scripts/test/tailnet-access/grants-e2e.sh

test-oidc-e2e:
	sh scripts/test/oidc/e2e-docker.sh

# On demand: examples/compose/control on the local docker (TLS edge, headscale, node-control, a client).
test-compose-control:
	sh scripts/test/compose-control/smoke-docker.sh

# Plan checks of the AWS control host module with a mocked provider (no account needed).
test-control-host-aws:
	cd terraform/modules/control-host/aws && terraform init -backend=false -input=false >/dev/null && terraform test

.PHONY: check-statestore-port
check-statestore-port:
	./scripts/check-statestore-port.sh

# Before 1.0.0 a new feature is off by default (#200): no new `<name>_enabled: true` role default outside the allowlist.
check-feature-defaults:
	scripts/check-feature-defaults.sh

test-feature-defaults:
	bash scripts/test/feature-defaults/run.sh

# Before a tag (#192): every role in check mode, every image built, every example overlay built and its fly.toml
# process commands started (also those of the previous release). REF=<tag or commit> (default HEAD), STEPS, OS.
release-preflight:
	scripts/release-preflight.sh $(or $(REF),HEAD)

lint:
	shellcheck -S warning -x scripts/*.sh scripts/test/tf-run/run_tests.sh scripts/test/check-denylist/run_tests.sh \
	  scripts/test/exit-node-state/run_tests.sh scripts/test/runtime-config/run_tests.sh scripts/test/headscale-policy/run.sh scripts/test/headscale-policy/e2e-docker.sh scripts/test/bridge-policy/e2e-docker.sh scripts/test/policy-apply/e2e-docker.sh examples/compose/control/scripts/policy-apply.sh apps/exit-node/*.sh \
	  scripts/tasks.sh scripts/test/tasks/run.sh scripts/release-preflight.sh scripts/deploy-fly.sh scripts/fly-snapshot.sh scripts/fleet-run.sh scripts/test/fleet-run/run_tests.sh scripts/test/fly-snapshot/run_tests.sh scripts/test/site-mk/run_tests.sh examples/private-repo/fly/deploy.sh scripts/test/ansible-os/run.sh scripts/test/ssm-state/e2e-docker.sh scripts/test/ssm-state/vm-e2e.sh \
	  scripts/headscale-tag-node.sh scripts/test/headscale-tag-node/run.sh \
	  .githooks/pre-commit .githooks/commit-msg \
	  apps/tailguard/*.sh apps/headscale/*.sh scripts/test/feature-defaults/run.sh scripts/check-enroll-paths.sh
	./scripts/check-feature-defaults.sh
	./scripts/check-enroll-paths.sh
	-./scripts/check-statestore-port.sh

# Helm chart charts/tailnet-connector (#103): helm lint, helm template | kubeconform (docker images if not installed).
test-k8s-connector:
	bash scripts/test/k8s-connector/validate.sh

# The chart in kind against the pinned headscale: join, route approval by policy, a ClusterIP from the tailnet.
test-k8s-connector-e2e:
	sh scripts/test/k8s-connector/e2e-kind.sh

# On demand (#123): the Access page (roles and grants) against a real headscale, two tailnet users.
test-access-e2e:
	sh scripts/test/access/e2e-docker.sh

# #194: mk/site.mk and scripts/deploy-fly.sh in a throwaway site repo (fake flyctl, no network).
test-site-mk:
	bash scripts/test/site-mk/run_tests.sh

# #197: a volume snapshot before each headscale deploy (fake flyctl).
test-fly-snapshot:
	bash scripts/test/fly-snapshot/run_tests.sh

# #199: fleet Ansible runs that skip nodes that are down and list them as pending (real ansible, no hosts).
test-fleet-run:
	bash scripts/test/fleet-run/run_tests.sh

# #213: tailscaled state in SSM keeps the node across a new instance (headscale + LocalStack).
test-ssm-state-e2e:
	bash scripts/test/ssm-state/e2e-docker.sh

# #213: the exit-node-vm/aws user data on systemd hosts: drop-in, pinned install, same node after a new host.
test-ssm-state-vm-e2e:
	bash scripts/test/ssm-state/vm-e2e.sh

# --- Go / config linting (#160) --------------------------------------------------------------
# Each tool runs natively when installed, else through its pinned docker image.
GO_APPS              := $(patsubst %/go.mod,%,$(wildcard apps/*/go.mod))
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION  := v1.9.0
LINT_BASE_REV        ?= origin/main
HADOLINT_VERSION     := v2.12.0
KUBECONFORM_VERSION  := v0.6.7
ACTIONLINT_VERSION   := 1.7.7
DOCKERFILES          := $(shell git ls-files '*Dockerfile*')
# git dir mounted into the golangci-lint container so --new-from-rev works in worktrees too
GIT_COMMON_DIR       := $(abspath $(shell git rev-parse --git-common-dir))
# govulncheck scans with the toolchain the images ship (FROM golang:1.27.3 in the Dockerfiles)
GO_VULN_TOOLCHAIN    ?= go1.27.3

# Caches of the golangci-lint container: docker volumes by default. With LINT_CACHE_DIR=<dir> (CI) they are host
# directories, so actions/cache can keep them between runs.
LINT_CACHE_MOUNTS = $(if $(LINT_CACHE_DIR),\
  -v "$(LINT_CACHE_DIR)/golangci":/root/.cache -v "$(LINT_CACHE_DIR)/gomod":/go/pkg/mod,\
  -v headlamp-golangci-cache:/root/.cache -v headlamp-gomod-cache:/go/pkg/mod)

GOLANGCI_LINT = $(if $(shell command -v golangci-lint 2>/dev/null),golangci-lint,\
  docker run --rm -v "$(CURDIR)":"$(CURDIR)" -v "$(GIT_COMMON_DIR)":"$(GIT_COMMON_DIR)" -w "$(CURDIR)/$$app" \
    $(LINT_CACHE_MOUNTS) \
    -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
    golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint)

.PHONY: lint-go lint-go-report vulncheck lint-config

lint-go:
	@rc=0; for app in $(GO_APPS); do echo "== golangci-lint (new since $(LINT_BASE_REV)) $$app"; \
	  (cd $$app && $(GOLANGCI_LINT) run --config ../../.golangci.yml --new-from-rev=$(LINT_BASE_REV) ./...) || rc=1; \
	done; exit $$rc

lint-go-report:
	@for app in $(GO_APPS); do echo "== golangci-lint (full report, warnings only) $$app"; \
	  (cd $$app && $(GOLANGCI_LINT) run --config ../../.golangci.yml --issues-exit-code=0 ./...) || echo "WARNING: golangci-lint failed to run in $$app"; \
	done

vulncheck:
	@rc=0; for app in $(GO_APPS); do echo "== govulncheck $$app"; \
	  if command -v govulncheck >/dev/null 2>&1; then (cd $$app && GOTOOLCHAIN=$(GO_VULN_TOOLCHAIN) govulncheck ./...) || rc=1; \
	  else (cd $$app && GOTOOLCHAIN=$(GO_VULN_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...) || rc=1; fi; \
	done; exit $$rc

lint-config:
	@rc=0; echo "== actionlint"; \
	if command -v actionlint >/dev/null 2>&1; then actionlint || rc=1; \
	else docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:$(ACTIONLINT_VERSION) -color || rc=1; fi; \
	echo "== hadolint"; \
	if command -v hadolint >/dev/null 2>&1; then hadolint --failure-threshold error $(DOCKERFILES) || rc=1; \
	else for f in $(DOCKERFILES); do echo "-- $$f"; docker run --rm -i hadolint/hadolint:$(HADOLINT_VERSION) hadolint --failure-threshold error - < $$f || rc=1; done; fi; \
	echo "== kubeconform"; \
	for c in charts/*/; do echo "-- $$c"; helm template lint $$c > /tmp/hl-kubeconform.yaml || { rc=1; continue; }; \
	  if command -v kubeconform >/dev/null 2>&1; then kubeconform -strict -summary -ignore-missing-schemas /tmp/hl-kubeconform.yaml || rc=1; \
	  else docker run --rm -i ghcr.io/yannh/kubeconform:$(KUBECONFORM_VERSION) -strict -summary -ignore-missing-schemas - < /tmp/hl-kubeconform.yaml || rc=1; fi; \
	done; exit $$rc

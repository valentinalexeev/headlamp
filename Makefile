# Developer targets for the headlamp core repo. Site operations (terraform, ansible, deploys) live in
# YOUR private repo: see examples/private-repo/ and CLAUDE.md.
.PHONY: hooks test-tf-run test-check-denylist test-exit-node-state test-headscale-policy test-headscale-policy-e2e test-runtime-config lint \
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

# on demand: needs docker and a headscale image (see the script)
test-headscale-policy-e2e:
	sh scripts/test/headscale-policy/e2e-docker.sh

.PHONY: check-statestore-port
check-statestore-port:
	./scripts/check-statestore-port.sh

lint:
	shellcheck -S error -x scripts/*.sh scripts/test/tf-run/run_tests.sh scripts/test/check-denylist/run_tests.sh \
	  scripts/test/exit-node-state/run_tests.sh scripts/test/runtime-config/run_tests.sh scripts/test/headscale-policy/run.sh scripts/test/headscale-policy/e2e-docker.sh apps/exit-node/*.sh \
	  scripts/tasks.sh scripts/test/tasks/run.sh \
	  scripts/headscale-tag-node.sh scripts/test/headscale-tag-node/run.sh \
	  .githooks/pre-commit .githooks/commit-msg \
	  apps/tailguard/*.sh apps/headscale/*.sh
	-./scripts/check-statestore-port.sh

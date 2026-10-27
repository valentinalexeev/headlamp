# Developer targets for the headlamp core repo. Site operations (terraform, ansible, deploys) live in
# YOUR private repo: see examples/private-repo/ and CLAUDE.md.
.PHONY: hooks test-tf-run test-check-denylist test-exit-node-state test-headscale-policy test-headscale-policy-e2e lint \
  test-tasks-cli test-task-runner test-node-control test-headscale-tag-node

hooks:
	@git config core.hooksPath .githooks

test-tf-run:
	./scripts/test/tf-run/run_tests.sh

test-check-denylist:
	./scripts/test/check-denylist/run_tests.sh

test-exit-node-state:
	./scripts/test/exit-node-state/run_tests.sh

test-headscale-policy:
	sh scripts/test/headscale-policy/run.sh

test-headscale-tag-node:
	sh scripts/test/headscale-tag-node/run.sh

test-tasks-cli:
	sh scripts/test/tasks/run.sh

test-task-runner:
	cd apps/task-runner && go vet ./... && go test -race -count=1 ./...

test-node-control:
	cd apps/fly/node-control && go vet ./... && go test -race -count=1 ./...

# on demand: needs docker and a headscale image (see the script)
test-headscale-policy-e2e:
	sh scripts/test/headscale-policy/e2e-docker.sh

.PHONY: check-statestore-port
check-statestore-port:
	./scripts/check-statestore-port.sh

lint:
	shellcheck -S error -x scripts/*.sh scripts/test/tf-run/run_tests.sh scripts/test/check-denylist/run_tests.sh \
	  scripts/test/exit-node-state/run_tests.sh scripts/test/headscale-policy/run.sh scripts/test/headscale-policy/e2e-docker.sh apps/fly/exit-ams/*.sh \
	  scripts/tasks.sh scripts/test/tasks/run.sh \
	  scripts/headscale-tag-node.sh scripts/test/headscale-tag-node/run.sh \
	  .githooks/pre-commit .githooks/commit-msg \
	  apps/tailguard/*.sh apps/fly/headscale/*.sh
	-./scripts/check-statestore-port.sh

# Developer targets for the headlamp core repo. Site operations (terraform, ansible, deploys) live in
# YOUR private repo: see examples/private-repo/ and CLAUDE.md.
.PHONY: hooks test-tf-run test-check-denylist lint

hooks:
	@git config core.hooksPath .githooks

test-tf-run:
	./scripts/test/tf-run/run_tests.sh

test-check-denylist:
	./scripts/test/check-denylist/run_tests.sh

lint:
	shellcheck -S error -x scripts/*.sh scripts/test/tf-run/run_tests.sh scripts/test/check-denylist/run_tests.sh \
	  .githooks/pre-commit .githooks/commit-msg \
	  apps/tailguard/*.sh apps/fly/headscale/*.sh

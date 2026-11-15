# ADR 0004: Test frameworks for shell scripts (bats-core) and end-to-end tests (Go)

Status: accepted. The owner accepted the proposal on #368 (bats-core, Go for end-to-end, the plan below). Milestone 0.21.0
for step 1, 0.22.0 for the rest. All of it is live in CI before 1.0.0.

## Context

The repository has about 57 test scripts written by hand in shell: about 38 unit tests of shell scripts (with fake
`curl`, `flyctl` or `sops` on `PATH`) and about 19 end-to-end tests on Docker or kind. Each one has its own helpers to
run a command, keep its exit code, compare output, count failures and remove temp files.

Rule 8 of ADR 0002 asks every script to start with `set -eu`. The test scripts cannot do this. They read exit codes
with `cmd; echo $?` and expect some commands to fail, so `-e` stops them at the first expected failure (#368: 11 suites
broke). The test scripts also differ in output, cleanup and skip handling.

## Decision

1. **Unit tests of shell scripts use [bats-core](https://github.com/bats-core/bats-core)**, with bats-support and
   bats-assert. The three are git submodules under `test/bats`, pinned to release tags. The tests live in
   `test/scripts/<name>.bats` and load `test/helpers.bash`. `run` keeps the exit code and the output of a command, so the
   script under test and the test file can both use `set -eu`. Each test gets its own temp directory
   (`BATS_TEST_TMPDIR`), which bats removes.
2. **End-to-end tests use Go** (`testing`, `os/exec`), in one module `tests/e2e`. A test calls `t.Skip` when Docker or
   kind is missing, and `t.Cleanup` removes its containers. `go test ./...` in that module runs them all.
3. `make test-sh` runs every bats test. A target that runs bats first checks out the submodules if they are missing.
4. A moved script gets `set -eu` (bash: `set -euo pipefail`) in the same pull request. Rule 8 of ADR 0002 then holds
   with no exception.

## Plan

| Step | Milestone | Content |
|---|---|---|
| 1 | 0.21.0 | bats under `test/bats`, `test/helpers.bash`, `make test-sh`, the CI step, and two scripts as the pattern (`check-denylist`, `fleet-check`) |
| 2 | 0.22.0 | the other unit test scripts, one pull request for each area |
| 3 | 0.22.0 | `tests/e2e` in Go, first `tailnet-dns`, then the other end-to-end scripts |
| 4 | before 1.0.0 | no test script left under `scripts/test`, and `set -eu` in every shell script |

## Consequences

- One way to write a shell test. A failure shows the command, its exit code and its output.
- Bash 3.2 or later is needed to run the unit tests (macOS has it). The scripts under test keep their own shell.
- A clone needs the submodules for the unit tests. `make` fetches them, and a plain `git clone` does not.
- The end-to-end tests need Go, which the repository needs anyway.

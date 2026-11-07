# ADR 0002: Adopt a subset of TigerStyle for the code of headlamp

Status: proposed. The owner accepts it when the pull request merges. Milestone 2.0.0.

## Context

[TigerStyle](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/TIGER_STYLE.md) is the code style of
TigerBeetle. Its order of goals is safety, then performance, then developer experience. Its main rules are:

- Put a limit on all things: loops, queues, input sizes, retries and timeouts.
- Use assertions to check the arguments, the return values and the invariants of each function.
- Keep the control flow simple. Keep each function at 70 lines or less.
- Allocate all memory at startup.
- Handle all errors.
- Use full words in names. Put the unit last: `timeout_ms`, `body_size_max_bytes`.
- Keep lines at 100 columns or less.
- Use as few dependencies as possible.

We measured the Go code of `apps/` at commit `bc928ef` against these rules. The scan did not include tests and
`cmd/console-demo`. The scan found 155 files and approximately 34,000 lines.

| Rule | Result |
|---|---|
| Limits on input | 18 of 19 `io.ReadAll` calls use `io.LimitReader`. The last call is after `http.MaxBytesReader`. |
| Timeouts | All `http.Client` values have a `Timeout`. All servers set `ReadHeaderTimeout`. There are 76 `context.WithTimeout` calls. |
| Limits on stored data | Constants limit the run history, secrets, search results and state size. |
| Assertions | There are 8 `panic` calls in 1,470 functions. No call checks an invariant. |
| Function length | 30 functions have more than 70 lines. The longest has 167 lines. |
| Loops | There are 22 `for {}` loops. Most stop when a context stops. Some algorithm and retry loops have no visible limit. |
| Errors | There are 80 `_ =` discards. Some do not have a comment that gives the reason. |
| Names | Names use abbreviations (`cur`, `aerr`, `hc`, `rs`). Byte limits use different names (`maxBody`, `maxBodyBytes`) and literals (`4<<20`). |
| Line length | 1,654 lines (approximately 5%) have more than 100 columns. |
| Dependencies | `node-control` has 12 direct modules. `task-runner` and `ingress-agent` have none. |
| Shell scripts | Some scripts do not set `-e`, `-u` or `pipefail`. `make lint` runs `shellcheck -S error`, so it does not show warnings. |
| Tests | Tests have approximately 43,000 lines. There are no fuzz tests. |

The code already applies the rules about limits and timeouts, but nothing does a check of them. The code does not
apply the rules about assertions and function length. These rules prevent the most errors in a long-running
service.

Two rules do not apply well to this repository:

- Static allocation. The apps are Go services that use the Go garbage collector. They do not have a hot path that
  is sensitive to latency.
- Zero dependencies. `tailscale.com` and the cloud SDKs are the reason that `node-control` and `funnel-ingress`
  exist. We cannot remove them.

## Decision

The Go code and the shell scripts of headlamp use the subset of TigerStyle below. These rules apply to new code and
to code that a change edits. Existing code does not need a change before it gets other work. Issues of milestone
2.0.0 hold the cleanup work.

### Rules that we adopt

1. **Limits.** Each loop, queue, buffer, retry, input and response has an explicit limit. A loop that must run
   until its context stops has a comment that says so. A retry loop has a maximum count or a deadline.
2. **Assertions on invariants.** Check the invariants of state that has more than one part. An example is the
   maps of the task engine. Use one helper for each module: `invariant(condition, message)`. In a server, the
   helper logs the failure, increments a metric and returns an error. It does not stop the process. In tests and in
   short-lived commands, it panics.
3. **Errors.** Handle each error. If the code must ignore an error, put a comment on the same line that gives the
   reason.
4. **Function length.** A new function has 70 lines or less. If a change makes a function longer than 70 lines,
   divide it in the same change.
5. **Names.** Use full words in names, except for the usual Go names (`ctx`, `err`, `w`, `r`, `t`, `i`). Put the
   unit last in a name: `bodySizeMaxBytes`, `retryCountMax`. Give each limit a named constant. Do not put a byte
   literal such as `4<<20` at a call site.
6. **Line length.** Keep Go lines at 100 columns or less. Use a tab width of 4 to count.
7. **Dependencies.** Add a new module only when the standard library or an existing dependency cannot do the
   work. Give the reason in the pull request.
8. **Shell scripts.** Each script starts with `set -eu`. Bash scripts also set `-o pipefail`. `shellcheck` shows
   warnings, not only errors.
9. **Fuzz tests.** A function that parses data from an operator or from the network has a Go fuzz test. The first
   targets are `tasks.ParseConfig`, `enroll.parseIngress` and the state store importer.

### Rules that we do not adopt

- Static allocation at startup. We give a capacity to `make` when we know the size, but we do not prevent
  allocation after startup.
- Zero dependencies. Rule 7 above controls new dependencies.
- The `snake_case` names of Zig. Go code uses Go names.

### Checks

`golangci-lint` does the check of the rules in CI, with these linters:

| Rule | Linter |
|---|---|
| 3 | `errcheck`, with a short list of allowed calls |
| 4 | `funlen` (70 lines) and `gocognit` |
| 6 | `lll` (100 columns, tab width 4) |
| 1, 5 | `mnd` for literals in calls, and review |

First, CI shows the results for existing code as warnings. Then each cleanup issue makes one linter strict for one
app. Rules 1, 2 and 7 need review. The independent reviewer of each pull request does a check of them.

## Consequences

- New code has a clear rule for limits, assertions, errors and size. The reviewer can refer to a rule number.
- The cleanup of the 30 long functions and of the shared importer changes code that has tests. It does not change
  behavior. Each part is a separate pull request with a `refactor` type.
- An invariant failure in a server does not stop the service. It shows as a log line and a metric, so an operator
  must monitor them.
- The lint step in CI takes more time.
- This ADR does not change the user-visible behavior of any image.

## Work in milestone 2.0.0

1. Add `golangci-lint` with the linters above, in warning mode. Make `shellcheck` show warnings.
2. Add `set -eu` (and `pipefail` for bash) to all shell scripts.
3. Add the `invariant` helper. Use it first in the task engine and in the enrolment registry.
4. Add fuzz tests for the three parsers in rule 9.
5. Divide the five longest functions. Move the duplicate state store importer to one place.
6. Give a limit to each loop that has no limit. Fix each `_ =` discard that has no reason, or add the reason.
7. Make each linter strict for each app when its warnings are at zero.

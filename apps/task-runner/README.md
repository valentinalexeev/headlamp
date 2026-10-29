# task-runner

A small generic service that runs a caller-supplied script in a throwaway
rootless container (Chromium + Playwright) and returns a small JSON result. It
runs on a node and is called over the tailnet (by node-control): start a run,
poll it, cancel it, read health. One run at a time. State is in memory only.

## API

All routes need `Authorization: Bearer <RUNNER_TOKEN>`; a wrong or missing
token gives an empty `401`. Unknown routes give an empty `404`.

| Request | Response |
|---|---|
| `POST /v1/runs` `{"script":"<js>","params":{},"secrets":{"NAME":"value"},"timeout_seconds":180}` | `201 {"id":"..."}`; `409` while a run is active (also while a cancelled run is still being reaped); `422` for an empty script, `timeout_seconds` outside 1..1800 (default 180), `params` that is not an object, `secrets` that is not an object of strings, wrong JSON types, trailing data, `params` or `secrets` over 100000 bytes encoded, or a body over 1 MiB |
| `GET /v1/runs/{id}` | `{"state":"running\|succeeded\|failed\|cancelled","result":{...}\|null,"error":"","log_tail":"<last 4 KB>"}`; `404` for an unknown id, also after a restart |
| `DELETE /v1/runs/{id}` | `204` (also for a run that already finished, which is left as it is); `404` for an unknown id. A running run is marked `cancelled` at once and its container is killed (`kill task-<id>`); the run slot frees when the container is reaped |
| `GET /v1/health` | `{"version":"...","busy":false,"mem_available_mb":N}` (`-1` when `/proc/meminfo` is unreadable) |
| `POST /v1/sessions` `{"ttl_seconds":3600,"idle_seconds":900}` (both optional) | `201 {"id","port","expires_at"}` (RFC 3339 UTC); `409` while a run or another session holds the slot; `422` for `ttl_seconds` outside 60..14400, `idle_seconds` outside 60..3600, bad JSON or trailing data; `404` when sessions are disabled (`RUNNER_SESSION_BIND` unset) |
| `GET /v1/sessions` | `{"session":null}` or `{"session":{"id","state":"running\|ended","started_at","expires_at","port","reason":""}}`; `reason` is `ttl`, `idle`, `stopped` or `failed` once ended; the last ended session is kept for 5 minutes |
| `DELETE /v1/sessions/{id}` | `204` (also for an ended session); `404` for an unknown id. The container is killed and removed; the slot frees when it is reaped |

A session is an interactive browser (see Image) and shares the single slot with runs: a session while a run is
active, or a run while a session is active, gets `409`. `busy` in `/v1/health` is true while a session runs.

Finished runs are kept for an hour (at most the last 20).

## Script contract

The script is a CommonJS module exporting `run` (`module.exports = {run}` or
`module.exports = fn`). `run({page, params, secrets, log})` gets a fresh
Playwright page in a fresh browser context; `log(msg)` writes a line to stderr
(it ends up in `log_tail`, readable only from this API). The return value must be a JSON object of at most
4096 bytes; otherwise the run is `failed` with the reason. A non-zero exit is
`failed` with the last output lines and `exit status N`; exceeding the timeout
kills the container and is `failed` with a message containing "timeout". A missing, unreadable or non-object result
(including a script that returns nothing) is `failed` with the reason. The runner does not scrub `result`: it is the
caller's to treat as sensitive (node-control scrubs secret values out of it).

Every string the runner returns in `error` and `log_tail` has each secret value of 4 or more
characters replaced by `***` (also in its JSON-escaped forms, and a secret cut by the log window is trimmed).

## Isolation

Per run: `podman --cgroup-manager=cgroupfs run --rm --name task-<id>
--read-only --cap-drop=ALL --security-opt no-new-privileges --pids-limit N
--memory M --timeout T --tmpfs /tmp`, where `T` is the run timeout plus a few seconds of grace (podman's own backstop;
the runner enforces the real deadline itself and kills first), the script mounted read-only at `/run/task` and a
writable result dir at `/run/result`, both in a 0700 temp dir under
`RUNNER_STATE_DIR` that is removed afterwards (and any stale `run-*` dirs at
startup, as a previous process may have left them). After a timeout or cancel the runner also runs `rm -f --time 0 task-<id>`
(twice, best effort), since the container outlives its client. Secrets and params go in as `-e TASK_SECRETS_JSON -e
TASK_PARAMS_JSON` with no value on the command line; the values are set only in
the environment of that one child process (which does not inherit `RUNNER_*`, so the bearer token never reaches it). The runner writes no secret to its own
files, but the values are in its memory (and swap, if enabled), and while a container exists podman keeps its
configuration, environment included, in the service user's storage. The `--cgroup-manager=cgroupfs`
global flag is required for a service user without a login session.

A session container gets the same hardening plus `--init`, `--shm-size=512m`, a writable tmpfs home and one
published port: `-p <RUNNER_SESSION_BIND>:<port>:<port>` (noVNC). The host part is always a specific IP; an empty,
invalid or unspecified `RUNNER_SESSION_BIND` is never published (empty disables sessions, the rest is a startup
error). The guard still applies to the session's uid, so the browser reaches the public internet only (not the node,
not the tailnet, not the runner API). The noVNC port is the one published port and has no password by design: the
tailnet ACL gates it, and nothing is stored (no profile, no history beyond the container's tmpfs). The container
is removed when the TTL passes, when it exits after the idle period, on `DELETE` and on runner shutdown.

## Configuration (environment)

| Variable | Default | |
|---|---|---|
| `RUNNER_TOKEN` | | required, bearer token |
| `RUNNER_IMAGE` | | required, image to run, pinned by digest |
| `RUNNER_LISTEN` | `127.0.0.1:9100` | bind address (set to the tailnet address) |
| `RUNNER_STATE_DIR` | `/var/lib/task-runner` | |
| `RUNNER_CONTAINER_BIN` | `podman` | tests point it at a fake |
| `RUNNER_MEMORY` | `1g` | container memory limit |
| `RUNNER_PIDS_LIMIT` | `512` | |
| `RUNNER_SESSION_BIND` | | host IP (the tailnet address) to publish the session port on; empty disables sessions |
| `RUNNER_SESSION_PORT` | `6080` | noVNC port, published and used inside the container |

Logs are JSON on stderr and carry run ids, states and durations only. On SIGTERM or SIGINT the runner cancels the
active run or session, waits for it to be reaped (up to 30 s) and exits.

## Image

`image/Containerfile` builds from the official Playwright image and installs a
pinned `playwright`; `host.js` is the entrypoint program (`node /app/host.js`).
`BASE` has no default on purpose: a build without
`--build-arg BASE=<playwright base image>@sha256:...` fails, so the base
cannot change under a running node. Pick the base image tag whose Playwright
version matches the pinned `playwright` package (1.64.1 in `image/Containerfile`)
and resolve it to a digest at deploy time.

The image also carries the browser session tools (Xvfb, openbox, x11vnc, noVNC).
`sh /app/session.sh` starts a headed Chromium on a virtual display and serves it
through noVNC on `SESSION_NOVNC_PORT`; x11vnc listens on loopback only and has no
password, so access control is the network ACL in front of that port. The
container exits 0 after `SESSION_IDLE_SECONDS` without a connected client (counted
from start, and after each disconnect) and non-zero if any child process dies. The
browser profile lives in tmpfs under `/tmp` only. `sh image/session_test.sh` runs
static checks on the script.

    podman build --build-arg BASE=<image@sha256:digest> -t task-run image/

## Develop

    go vet ./... && go test -race ./...

Tests drive the real HTTP handler against a fake container binary (a shell
script that records argv and env, writes the result, sleeps, fails or ignores
`kill`).

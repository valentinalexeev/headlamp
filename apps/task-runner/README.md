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
| `POST /v1/runs` `{"script":"<js>","params":{},"secrets":{"NAME":"value"},"timeout_seconds":180}` | `201 {"id":"..."}`; `409` while a run is active; `422` for an empty script, `timeout_seconds` outside 1..1800 (default 180), wrong JSON types or a body over 1 MiB |
| `GET /v1/runs/{id}` | `{"state":"running\|succeeded\|failed\|cancelled","result":{...}\|null,"error":"","log_tail":"<last 4 KB>"}`; `404` for an unknown id, also after a restart |
| `DELETE /v1/runs/{id}` | `204`; kills the container (`kill task-<id>`) |
| `GET /v1/health` | `{"version":"...","busy":false,"mem_available_mb":N}` (`-1` when `/proc/meminfo` is unreadable) |

Finished runs are kept for an hour (at most the last 20).

## Script contract

The script is a CommonJS module exporting `run` (`module.exports = {run}` or
`module.exports = fn`). `run({page, params, secrets, log})` gets a fresh
Playwright page in a fresh browser context; `log(msg)` writes a line to stderr
(it ends up in `log_tail`). The return value must be a JSON object of at most
4096 bytes; otherwise the run is `failed` with the reason. A non-zero exit is
`failed` with the last output lines and `exit status N`; exceeding the timeout
kills the container and is `failed` with a message containing "timeout".

Every string returned (`error`, `log_tail`) has each secret value of 4 or more
characters replaced by `***`.

## Isolation

Per run: `podman --cgroup-manager=cgroupfs run --rm --name task-<id>
--read-only --cap-drop=ALL --security-opt no-new-privileges --pids-limit N
--memory M --timeout T --tmpfs /tmp`, the script mounted read-only at `/run/task` and a
writable result dir at `/run/result`, both in a 0700 temp dir under
`RUNNER_STATE_DIR` that is removed afterwards (and any stale `run-*` dirs at
startup). After a timeout or cancel the runner also runs `rm -f --time 0 task-<id>`
(twice, best effort), since the container outlives its client. Secrets and params go in as `-e TASK_SECRETS_JSON -e
TASK_PARAMS_JSON` with no value on the command line; the values are set only in
the environment of that one child process. The `--cgroup-manager=cgroupfs`
global flag is required for a service user without a login session.

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

Logs are JSON on stderr and carry run ids, states and durations only.

## Image

`image/Containerfile` builds from the official Playwright image and installs a
pinned `playwright`; `host.js` is the entrypoint program (`node /app/host.js`).
`BASE` has no default on purpose: a build without
`--build-arg BASE=mcr.microsoft.com/playwright@sha256:...` fails, so the base
cannot change under a running node (the version tag the pinned `playwright`
matches is v1.64.1-noble; resolve it to a digest at deploy time).

    podman build --build-arg BASE=<image@sha256:digest> -t task-run image/

## Develop

    go vet ./... && go test -race ./...

Tests drive the real HTTP handler against a fake container binary (a shell
script that records argv and env, writes the result, sleeps, fails or ignores
`kill`).

# task-runner

A small generic service. It runs a script that the caller supplies in a throwaway
rootless container (Chromium + Playwright). It returns a small JSON result. It runs on a node.

The caller (node-control) calls it over the tailnet. The caller can start a run, poll it, cancel it and
read health. The service does one run at a time. State is in memory only.

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

A session is an interactive browser (see Image). It shares the single slot with runs. A session that starts while a run is
active gets `409`. A run that starts while a session is active also gets `409`. `busy` in `/v1/health` is true while a session runs.

Finished runs are kept for an hour (at most the last 20).

## Script contract

The script is a CommonJS module that exports `run` (`module.exports = {run}` or
`module.exports = fn`). `run({page, params, secrets, log})` gets a fresh
Playwright page in a fresh browser context. `log(msg)` writes a line to stderr.
The line goes to `log_tail`, which you can read only from this API.

The return value must be a JSON object of at most
4096 bytes. Otherwise the run is `failed` with the reason. A non-zero exit is
`failed` with the last output lines and `exit status N`. If a run exceeds the timeout, the runner
kills the container, and the run is `failed` with a message that contains "timeout". A result that is missing, unreadable or not an object
is `failed` with the reason. This includes a script that returns nothing.

**WARNING:** Treat `result` as sensitive. The runner does not scrub `result`. node-control scrubs secret values out of it.

In every string that the runner returns in `error` and `log_tail`, the runner replaces each secret value of 4 or more
characters with `***`. This includes the JSON-escaped forms of the value. If the log window cuts a secret, the runner trims it.

## Isolation

Each run uses this command: `podman --cgroup-manager=cgroupfs run --rm --name task-<id>
--read-only --cap-drop=ALL --security-opt no-new-privileges --pids-limit N
--memory M --timeout T --tmpfs /tmp`. These rules also apply:

- `T` is the run timeout plus a few seconds of grace. This is the backstop of podman.
  The runner enforces the real deadline itself and kills first.
- The script is mounted read-only at `/run/task`. A writable result dir is at `/run/result`.
  Both are in a 0700 temp dir under `RUNNER_STATE_DIR`. The runner removes this dir afterwards.
  At startup, the runner also removes any stale `run-*` dirs, because a previous process can leave them.
- After a timeout or cancel, the runner also runs `rm -f --time 0 task-<id>` (twice, best effort),
  because the container outlives its client.
- The runner passes secrets and params as `-e TASK_SECRETS_JSON -e
  TASK_PARAMS_JSON`, with no value on the command line. It sets the values only in
  the environment of that one child process. That process does not inherit `RUNNER_*`, so the bearer token never reaches it.
- The runner writes no secret to its own files. The values are in its memory (and swap, if enabled).
  While a container exists, podman keeps its configuration, environment included, in the storage of the service user.
- The `--cgroup-manager=cgroupfs` global flag is required for a service user without a login session.

A session container gets the same hardening plus `--init`, `--shm-size=512m`, a writable tmpfs home and one
published port: `-p <RUNNER_SESSION_BIND>:<port>:<port>` (noVNC). The host part is always a specific IP.
The runner never publishes an empty, invalid or unspecified `RUNNER_SESSION_BIND`. An empty value disables sessions.
Any other such value is a startup error.

The guard still applies to the uid of the session. The browser therefore reaches the public internet only (not the node,
not the tailnet, not the runner API). The noVNC port is the one published port. The tailnet ACL controls who can reach it.
Every session also carries a random 128-bit token (see below).

The runner stores nothing else (no profile, no history beyond the tmpfs of the container).
The runner removes the container when the TTL passes, when it exits after the idle period, on `DELETE` and on runner shutdown.

A running session makes the node report `busy`. Scheduled tasks pinned to that node then fail with "node busy" or
"no eligible node" until the session ends (up to the TTL).

Trust model: the ACL controls reachability. The session token stops other pages and users on allowed devices from
using the endpoint. The token is random for each session, it is in the link, and it dies with the session.

The runner generates the token (`crypto/rand`, 32 hex chars). It passes the token to the container only as
`-e SESSION_TOKEN`, with the value in the environment of the child (never on a command line). It returns the token in the `POST /v1/sessions`
answer. It returns the token in `GET /v1/sessions` while the session is running, and never after the session ended.
The runner never logs the token. In the container, the token is only in the environment and in a 0600 file under `/tmp`.
websockify refuses a WebSocket that lacks `?token=`.

**WARNING:** Do not share the session link. The token is in the browser history of the operator and in the address bar of the page while the session is
open. Stop the session to invalidate the token.

Closing the last browser window reopens Chrome. With sessions disabled
(`RUNNER_SESSION_BIND` empty), all three session endpoints answer `404`.

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

Logs are JSON on stderr. They carry run ids, states and durations only. On SIGTERM or SIGINT, the runner cancels the
active run or session, waits up to 30 s for it to be reaped, and exits.

## Image

`image/Containerfile` builds from the official Playwright image and installs a
pinned `playwright`. `host.js` is the entrypoint program (`node /app/host.js`).
`BASE` has no default on purpose. A build without
`--build-arg BASE=<playwright base image>@sha256:...` fails, so the base
cannot change under a running node. Pick the base image tag whose Playwright
version matches the pinned `playwright` package (1.64.1 in `image/Containerfile`).
Resolve the tag to a digest at deploy time.

The image also carries the browser session tools (Xvnc of TigerVNC, openbox, noVNC).

- `sh /app/session.sh` starts a headed Google Chrome on a virtual display. The build installs Chrome into the image (linux/amd64).
  Playwright's Chromium is the fallback.
- The script serves the display through noVNC on `SESSION_NOVNC_PORT`.
- Xvnc (X server and VNC server in one) listens on loopback only and has no
  password. It accepts desktop resizing from the client. A noVNC client that uses `resize=remote` then gets a desktop of exactly
  the size of its browser tab. There is no maximum size. The memory limit of the container is the bound, so a very large tab
  costs memory.
- The network ACL controls access to the port. websockify connects only a WebSocket that carries the per-session
  `SESSION_TOKEN` (`?token=`, read from a token file).
- The container exits 0 after `SESSION_IDLE_SECONDS` without a connected client. The count uses established connections to
  the port of Xvnc, read from `/proc/net/tcp`. The count starts at container start and after each disconnect.
- The container exits non-zero if Xvnc or websockify dies. The script restarts openbox and Chrome when they close.
  The session fails only if one of them exits again at once five times in a row.
- The browser profile lives in tmpfs under `/tmp` only.

`sh image/session_test.sh` runs static checks on the script.

    podman build --build-arg BASE=<image@sha256:digest> -t task-run image/

## Develop

    go vet ./... && go test -race ./...

Tests drive the real HTTP handler against a fake container binary. This binary is a shell
script that records argv and env, writes the result, sleeps, fails or ignores
`kill`.

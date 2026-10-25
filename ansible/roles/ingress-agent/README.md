# ingress-agent

Deploys the Python ingress agent (`apps/ingress-agent/` in this repo) as the systemd service
`ingress-agent.service` on the Reality ingress host. The agent holds opaque credentials pushed
by the operator side over the tailnet, renders `/etc/xray/config.json` from
`/etc/xray/config.base.json`, validates it with `xray run -test`, replaces it atomically and
restarts `xray.service`. See `apps/ingress-agent/README.md` for its API.

## Relation to reality-ingress

Run `reality-ingress` first (and the base `tailscale` role): it installs xray, creates the
`xray` user and `/etc/xray` (mode 2750 root:xray, setgid) and writes `config.base.json`. The
defaults here (`xray_bin`, `xray_unit`, `xray_api`, `xray_base_config`, `xray_config`,
`xray_asset_dir`) match that role; override both together or neither. This role only warns if
the xray binary or the base config is missing, because on a fresh host in check mode they do
not exist yet. It never restarts xray itself: the agent does that when the credential set
changes. The unit is ordered `After=xray.service` but does not require it.

## Variables

| Variable | Default | Notes |
|---|---|---|
| `ingress_agent_src` | `""` | Required. Path on the **control host** to the `apps/ingress-agent` directory (its `app/` is copied). |
| `ingress_agent_token` | `""` | Required, vault. At least 32 characters, no whitespace, quotes, backslash, `$` or backtick. Never logged. |
| `ingress_agent_port` | `9000` | 1-65535, bound on the tailnet address only. |
| `ingress_agent_dir` | `/opt/ingress-agent` | Code lands in `<dir>/app/`. |
| `ingress_agent_state_dir` | `/var/lib/ingress-agent` | Must be `/var/lib/<dir>` (becomes `StateDirectory`, mode 0700). |
| `ingress_agent_env_file` | `/etc/ingress-agent/env` | Mode 0600 root:root. |
| `ingress_agent_fastapi_min` / `ingress_agent_uvicorn_min` | `0.110` / `0.30` | Floors checked against the OS packages. |
| `xray_bin`, `xray_unit`, `xray_api`, `xray_base_config`, `xray_config`, `xray_asset_dir` | see `defaults/main.yml` | Must match `reality-ingress`. `xray_asset_dir` is passed as `XRAY_LOCATION_ASSET` so `xray run -test` finds the geodata. |

## Public parameters and discovery (`GET /v1/info`)

The agent publishes how clients reach this ingress, so node-control can discover it and the operator can approve it
(no copying of host, key and short id). Public values only; the endpoint never returns a secret and the agent has no
variable for the Reality private key. Role variables (shared names with `reality-ingress`, defaults keep existing
deployments valid):

| Variable | Default | Env var in the agent |
|---|---|---|
| `ingress_name` | `""` | `INGRESS_NAME` (short label, `[a-z0-9-]`, at most 32 characters) |
| `ingress_host` | `""` | `INGRESS_HOST` |
| `ingress_listen_port` | `443` | `INGRESS_PORT` |
| `reality_sni` | `ingress_host` | `INGRESS_SNI` |
| `reality_short_id` | `""` | `INGRESS_SHORT_ID` |
| `reality_public_key` | `""` | `INGRESS_PUBLIC_KEY`: the public half of the vaulted `reality_private_key`; derive it with `xray x25519 -i <private key>` and set it as a variable. The role does not run xray for this. |

The six variables are exported only when `ingress_name`, `ingress_host`, `reality_public_key` and `reality_short_id`
are all set; otherwise `/v1/info` answers `503` and everything else works as before. If any of the four is set,
preflight validates all of them. Use a token per ingress host (`ingress_agent_token` differs on every host); the
operator approves a discovered ingress in node-control before it is used.

## Python dependencies

Installed from the OS packages (`python3-fastapi`, `python3-uvicorn`, `python3-pydantic`),
since hosts in restricted networks may not reach PyPI. `apps/ingress-agent/requirements.txt` asks for
`fastapi>=0.110,<1.0` and `uvicorn>=0.30,<1.0`; the role runs
`python3 -c "import fastapi, uvicorn, pydantic"`, prints the versions and fails with a clear
message if the import fails or fastapi/uvicorn are older than the floors. A distro whose
packages are older (e.g. Ubuntu 22.04) fails here by design; use a newer release.
`httpx` is a test-only dependency and is not installed.

## What it does

1. Preflight (before anything is written): required variables, token length/charset, port
   range, absolute paths, facts gathered, the source exists on the control host.
2. Installs the packages, copies `app/*.py` (no `__pycache__`, no tests; root, 0755/0644;
   anything else under the app directory on the host is removed; `ingress_agent_src` may contain `..`), installs the wrapper
   `/usr/local/sbin/ingress-agent-run` and the unit.
3. Writes the env file, applies handlers, enables and starts the service.

The wrapper waits up to 60 s for `tailscale ip -4` (the host's main tailscaled), exits 1 with
a message if there is none, then `exec`s
`python3 -m uvicorn app.main:create_app_from_env --factory --host <tailnet ip> --port <port> --no-access-log`.
It never binds 0.0.0.0. Check mode is safe: files are templated normally, package checks,
handlers and the service start are skipped.

## Security notes

- The token lives only in the 0600 env file. It is not in the unit, the wrapper or the process
  arguments (so not in `ps`); every task that handles it has `no_log: true`.
- The service runs as root because it must `systemctl restart xray`. Hardening:
  `NoNewPrivileges`, `ProtectSystem=strict` (writable: the state dir and `/etc/xray`),
  `ProtectHome`, `PrivateTmp`, `ProtectKernelTunables`, `ProtectControlGroups`,
  `RestrictSUIDSGID`. **Verify on the live host that `systemctl restart xray` works from inside
  this sandbox** (e.g. push credentials and check `journalctl -u ingress-agent`); if it does not,
  relax the sandbox rather than the listener.
- Reachability is limited by the tailnet ACL: allow `<port>` to this host from the operator
  side (node-control) only.
- `--no-access-log` keeps request details out of the journal.

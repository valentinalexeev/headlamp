# ingress-agent

This role deploys the Go bridge agent (`apps/ingress-agent/` in this repo) as the systemd service
`ingress-agent.service` on the Reality bridge host. The operator side pushes opaque credentials to the agent
over the tailnet. The agent holds them and renders `/etc/xray/config.json` from
`/etc/xray/config.base.json`. It validates the config with `xray run -test`, replaces it atomically and
restarts `xray.service`. See `apps/ingress-agent/README.md` for its API.

## Supported systems

The role uses `apt` and systemd. The OS matrix (`make test-ansible-os`, #170) tests it on Ubuntu 26.04 Server, Ubuntu 26.04 Minimal and Debian 13. The PR of #170 gives the result of each test.

For a bridge VM, use Ubuntu 26.04 Minimal. It is the preferred image. Use Debian 13 only when the OS matrix shows a pass for all bridge roles on it.

## Relation to reality-ingress

Run `reality-ingress` first (and the base `tailscale` role). It installs xray, creates the
`xray` user and `/etc/xray` (mode 2750 root:xray, setgid) and writes `config.base.json`. The
defaults here (`xray_bin`, `xray_unit`, `xray_api`, `xray_base_config`, `xray_config`,
`xray_asset_dir`) match that role. Override both together or neither.

This role only warns if
the xray binary or the base config is missing. On a fresh host in check mode, they do
not exist yet. The role never restarts xray itself. The agent restarts xray when the credential set
changes. The unit is ordered `After=xray.service` but does not require it.

## Variables

| Variable | Default | Notes |
|---|---|---|
| `ingress_agent_src` | `""` | Required. Path on the **control host** to the `apps/ingress-agent` directory. The role builds the agent from it there (Go 1.26 or later on the control host). |
| `ingress_agent_bin` | `/usr/local/bin/ingress-agent` | Where the binary goes on the bridge. |
| `ingress_agent_build_dir` | `$TMPDIR/ingress-agent-build` | Where the control host keeps the built binaries, one for each architecture. |
| `ingress_agent_token` | `""` | Required, vault. At least 32 characters, no whitespace, quotes, backslash, `$` or backtick. Never logged. |
| `ingress_agent_port` | `9000` | 1-65535, bound on the tailnet address only. |
| `ingress_agent_dir` | `/opt/ingress-agent` | The former Python agent was in `<dir>/app/`. The role removes that tree. |
| `ingress_agent_state_dir` | `/var/lib/ingress-agent` | Must be `/var/lib/<dir>` (becomes `StateDirectory`, mode 0700). |
| `ingress_agent_env_file` | `/etc/ingress-agent/env` | Mode 0600 root:root. |
| `xray_bin`, `xray_unit`, `xray_api`, `xray_base_config`, `xray_config`, `xray_asset_dir` | see `defaults/main.yml` | Must match `reality-ingress`. `xray_asset_dir` is passed as `XRAY_LOCATION_ASSET` so `xray run -test` finds the geodata. |

## Public parameters and discovery (`GET /v1/info`)

The agent publishes how clients reach this bridge. Node-control can then discover the bridge, and the operator can approve it
(no copying of host, key and short id).

The agent publishes public values only. The endpoint never returns a secret, and the agent has no
variable for the Reality private key. These are the role variables. Some names are shared with `reality-ingress`. The defaults keep existing
deployments valid:

| Variable | Default | Env var in the agent |
|---|---|---|
| `ingress_name` | `""` | `INGRESS_NAME` (short label, `[a-z0-9-]`, at most 32 characters) |
| `ingress_host` | `""` | `INGRESS_HOST` |
| `ingress_listen_port` | `443` | `INGRESS_PORT` |
| `reality_sni` | `ingress_host` | `INGRESS_SNI` |
| `reality_short_id` | `""` | `INGRESS_SHORT_ID` |
| `reality_public_key` | `""` | `INGRESS_PUBLIC_KEY`: the public half of the vaulted `reality_private_key`; derive it with `xray x25519 -i <private key>` and set it as a variable. The role does not run xray for this. |

The role exports the six variables only when `ingress_name`, `ingress_host`, `reality_public_key` and `reality_short_id`
are all set. Otherwise `/v1/info` answers `503` and everything else works as before.

Preflight validates the six
values only when `ingress_name` or `reality_public_key` is set. Then all of them must be valid, so a partial
configuration fails. Hosts that only have `ingress_host` and `reality_short_id` (required by `reality-ingress`) are
not affected. `/v1/info` stays `503` there until you set the two new variables.

Use a token for each bridge host (`ingress_agent_token` differs on every host). The
operator approves a discovered bridge in node-control before anyone uses it.

## No runtime on the bridge

The agent is one static Go binary (#133). The role builds it on the control host, for the architecture of the
bridge (`x86_64` or `aarch64`), and copies it. The bridge needs no Python, no package and no network access for
the agent. Before 0.11.0 the agent was a Python service. The role removes its tree, `<ingress_agent_dir>/app`.

## What it does

1. Preflight, before the role writes anything. It checks required variables, token length and charset, port
   range, absolute paths and gathered facts. It also checks that the source exists on the control host.
2. Build the agent on the control host and copy the binary to `ingress_agent_bin` (root, 0755).
3. Remove the tree of the former Python agent.
4. Install the wrapper `/usr/local/sbin/ingress-agent-run` and the unit.
5. Write the env file, apply handlers, and enable and start the service.

The wrapper waits up to 60 s for `tailscale ip -4` (the main tailscaled of the host). If there is none, it exits 1 with
a message. Then it `exec`s
the binary with `AGENT_LISTEN=<tailnet ip>:<port>`. It never binds 0.0.0.0. Check mode is safe: the role
templates files normally. It skips the handlers and the service start.

## Security notes

- The token lives only in the 0600 env file. It is not in the unit, the wrapper or the process
  arguments (so not in `ps`). Every task that handles it has `no_log: true`.
- The service runs as root, because it must `systemctl restart xray`. These are the hardening settings:
  `NoNewPrivileges`, `ProtectSystem=strict` (writable: the state dir and `/etc/xray`),
  `ProtectHome`, `PrivateTmp`, `ProtectKernelTunables`, `ProtectControlGroups`,
  `RestrictSUIDSGID`. **Make sure on the live host that `systemctl restart xray` works from inside
  this sandbox.** For example, push credentials and check `journalctl -u ingress-agent`. If it does not work,
  relax the sandbox, not the listener.
- The tailnet ACL limits reachability. Allow `<port>` to this host from the operator
  side (node-control) only.
- `--no-access-log` keeps request details out of the journal.

"""Fly Machine start/stop/status via Fly's Machines API.

Node config block: ``"fly": {"app": "<fly app name>"}``. The app is expected to run a single
machine; every call resolves the current machine by listing the app's machines, so no machine ID is
configured anywhere.

Credentials: a Fly deploy token scoped to that one app, in the FLY_API_TOKEN env var (a Fly secret).
Deploy tokens are app-scoped: the token can manage that app and nothing else.
"""

from __future__ import annotations

import os
from typing import Any

import requests

from .errors import APIError, NotConfigured

_API_BASE = "https://api.machines.dev/v1"
_TIMEOUT = 10


def _headers() -> dict[str, str]:
    token = os.environ.get("FLY_API_TOKEN")
    if not token:
        raise NotConfigured("FLY_API_TOKEN is not set")
    return {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}


def _app(node: dict[str, Any]) -> str:
    app = (node.get("fly") or {}).get("app")
    if not app:
        raise NotConfigured('node config needs "fly": {"app": ...}')
    return app


def _primary_machine(app: str) -> dict[str, Any]:
    try:
        resp = requests.get(f"{_API_BASE}/apps/{app}/machines", headers=_headers(), timeout=_TIMEOUT)
    except requests.RequestException as e:
        raise APIError(str(e)) from e
    if not resp.ok:
        raise APIError(f"listing machines: {resp.status_code} {resp.text}")
    machines = [m for m in resp.json() if m.get("state") != "destroyed"]
    if not machines:
        raise APIError(f"no machines found for app '{app}'")
    return machines[0]


def status(node: dict[str, Any]) -> dict[str, Any]:
    machine = _primary_machine(_app(node))
    return {"status": machine.get("state", "unknown"), "machine_id": machine.get("id")}


def _act(node: dict[str, Any], action: str, done: str, transient: str) -> tuple[str, str]:
    app = _app(node)
    machine = _primary_machine(app)
    previous = machine.get("state", "unknown")
    if previous == done:
        return previous, previous
    try:
        resp = requests.post(
            f"{_API_BASE}/apps/{app}/machines/{machine['id']}/{action}", headers=_headers(), timeout=_TIMEOUT
        )
    except requests.RequestException as e:
        raise APIError(str(e)) from e
    if not resp.ok:
        raise APIError(f"{action} machine: {resp.status_code} {resp.text}")
    return previous, transient


def start(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already started."""
    return _act(node, "start", "started", "starting")


def stop(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already stopped."""
    return _act(node, "stop", "stopped", "stopping")

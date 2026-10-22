"""Selectel Cloud (OpenStack) instance start/stop/status.

Node config block: ``"selectel": {"region": "<region>", "instance_name": "vega"}`` or ``{"instance_id": ...}``.

Which instance: by default found BY NAME in the project on every call (so replacing the VM needs no
config change); ``instance_id`` pins one instance and takes precedence. ``region`` falls back to the
SELECTEL_REGION env var.

Credentials (Fly secrets): SELECTEL_ACCOUNT_ID (the Keystone domain name), SELECTEL_PROJECT_ID,
SELECTEL_USER, SELECTEL_PASSWORD - a service user that is a member of the project. Talks plain REST to
Keystone and Nova, so no extra SDK is needed.

Status vocabulary is normalised to the other providers': ACTIVE -> RUNNING, SHUTOFF -> STOPPED.
"""

from __future__ import annotations

import os
import re
import threading
import time
from typing import Any

import requests

from .errors import APIError, NotConfigured

AUTH_URL = os.environ.get("SELECTEL_AUTH_URL", "https://cloud.api.selcloud.ru/identity/v3")
_TIMEOUT = 20
_STATUS = {"ACTIVE": "RUNNING", "SHUTOFF": "STOPPED", "BUILD": "STARTING", "HARD_REBOOT": "STARTING"}

_lock = threading.Lock()
_token: dict[str, Any] = {}


def _require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise NotConfigured(f"{name} is not set")
    return value


def _region(node: dict[str, Any]) -> str:
    region = (node.get("selectel") or {}).get("region") or os.environ.get("SELECTEL_REGION", "")
    if not region:
        raise NotConfigured('set "selectel": {"region": ...} (or the SELECTEL_REGION env var)')
    return region


def _auth(region: str) -> tuple[str, str]:
    """(token, compute endpoint) for the region; the token is cached until shortly before expiry."""
    with _lock:
        cached = _token.get(region)
        if cached and cached["expires"] - time.time() > 300:
            return cached["token"], cached["compute"]
        body = {"auth": {
            "identity": {"methods": ["password"], "password": {"user": {
                "name": _require_env("SELECTEL_USER"),
                "domain": {"name": _require_env("SELECTEL_ACCOUNT_ID")},
                "password": _require_env("SELECTEL_PASSWORD"),
            }}},
            "scope": {"project": {"id": _require_env("SELECTEL_PROJECT_ID")}},
        }}
        try:
            r = requests.post(f"{AUTH_URL}/auth/tokens", json=body, timeout=_TIMEOUT)
            r.raise_for_status()
            data = r.json()["token"]
        except requests.RequestException as e:
            raise APIError(f"keystone auth failed: {e}") from e
        endpoints = [
            ep["url"]
            for svc in data.get("catalog", []) if svc.get("type") == "compute"
            for ep in svc.get("endpoints", [])
            if ep.get("interface") == "public" and ep.get("region") == region
        ]
        if not endpoints:
            raise APIError(f"no public compute endpoint for region {region!r} in the catalog")
        # Nova may report a versioned or a bare URL; normalise to the v2.1 root.
        compute = endpoints[0].rstrip("/")
        _token[region] = {"token": r.headers["X-Subject-Token"], "compute": compute, "expires": time.time() + 3000}
        return _token[region]["token"], compute


def _call(node: dict[str, Any], method: str, path: str, **kw: Any) -> requests.Response:
    token, compute = _auth(_region(node))
    try:
        r = requests.request(method, f"{compute}{path}", headers={"X-Auth-Token": token}, timeout=_TIMEOUT, **kw)
        r.raise_for_status()
        return r
    except requests.RequestException as e:
        raise APIError(str(e)) from e


def _instance_id(node: dict[str, Any]) -> str:
    block = node.get("selectel") or {}
    if block.get("instance_id"):
        return block["instance_id"]
    name = block.get("instance_name")
    if not name:
        raise NotConfigured('node config needs "selectel": {"instance_name": ...} or {"instance_id": ...}')
    # Nova matches `name` as a regex; anchor it so "vega" never matches "vega2".
    servers = _call(node, "GET", "/servers", params={"name": f"^{re.escape(name)}$"}).json()["servers"]
    if not servers:
        raise APIError(f"no instance named {name!r} in the project")
    if len(servers) > 1:
        raise APIError(f"{len(servers)} instances named {name!r}; refusing to guess")
    return servers[0]["id"]


def _raw_status(node: dict[str, Any], instance_id: str) -> str:
    return _call(node, "GET", f"/servers/{instance_id}").json()["server"]["status"]


def status(node: dict[str, Any]) -> dict[str, Any]:
    raw = _raw_status(node, _instance_id(node))
    return {"status": _STATUS.get(raw, raw)}


def _power(node: dict[str, Any], want: str, action: str, transient: str) -> tuple[str, str]:
    instance_id = _instance_id(node)
    raw = _raw_status(node, instance_id)
    previous = _STATUS.get(raw, raw)
    if previous == want:
        return previous, previous
    _call(node, "POST", f"/servers/{instance_id}/action", json={action: None})
    return previous, transient


def start(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already RUNNING."""
    return _power(node, "RUNNING", "os-start", "STARTING")


def stop(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already STOPPED."""
    return _power(node, "STOPPED", "os-stop", "STOPPING")

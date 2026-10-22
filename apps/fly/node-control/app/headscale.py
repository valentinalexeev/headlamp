"""Actual tailnet state (as headscale itself sees it) for the nodes this app controls -- distinct
from, and a useful complement to, each node's cloud-provider status (a VM can be "RUNNING" while its
tailscale connection is flapping or its routes are unapproved, which the cloud-side status alone
can't show).

Credentials: a headscale API key, passed via the HEADSCALE_API_KEY env var (a Fly secret).
Read-only from this app's perspective: only ever calls the node-list endpoint.
"""

from __future__ import annotations

import os
from typing import Any

import requests

HEADSCALE_API_URL = os.environ.get("HEADSCALE_LOGIN_SERVER", "")
_TIMEOUT = 10

class HeadscaleNotConfigured(Exception):
    """Raised when required env vars are missing."""


class HeadscaleAPIError(Exception):
    """Raised when the headscale API call itself fails."""


def _headers() -> dict[str, str]:
    token = os.environ.get("HEADSCALE_API_KEY")
    if not token:
        raise HeadscaleNotConfigured("HEADSCALE_API_KEY is not set")
    return {"Authorization": f"Bearer {token}"}


def tailnet_status(tracked: set[str]) -> dict[str, Any]:
    """One entry per tracked tailnet name (the nodes' ``tailnetName`` from the config).

    A node's headscale name can carry a numeric suffix ("name-1") if it re-registered while its
    previous entry still existed. Entries are grouped by base name (before a trailing "-N") so a
    stale duplicate doesn't shadow the live one: an online entry wins, else the first seen.
    """
    try:
        resp = requests.get(
            f"{HEADSCALE_API_URL}/api/v1/node",
            headers=_headers(),
            timeout=_TIMEOUT,
        )
    except requests.RequestException as e:
        raise HeadscaleAPIError(str(e)) from e
    if not resp.ok:
        raise HeadscaleAPIError(f"listing nodes: {resp.status_code} {resp.text}")

    nodes = resp.json().get("nodes", [])
    result: dict[str, Any] = {}
    for node in nodes:
        name = node.get("name", "")
        base_name = name.split("-")[0] if "-" in name and name.split("-")[-1].isdigit() else name
        if base_name not in tracked:
            continue

        entry = {
            "name": name,
            "online": node.get("online", False),
            "last_seen": node.get("lastSeen"),
            "ip_addresses": node.get("ipAddresses", []),
            "approved_route_count": len(node.get("approvedRoutes", [])),
            "available_route_count": len(node.get("availableRoutes", [])),
        }

        existing = result.get(base_name)
        if existing is None or (entry["online"] and not existing["online"]):
            result[base_name] = entry

    return result

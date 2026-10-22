"""OCI Compute instance start/stop/status.

Node config block: ``"oci": {"region", "compartment_id", "instance_name", "instance_id"}``.

Which instance: by default it is found BY NAME inside the compartment (``compartment_id`` +
``instance_name``) on every call, so replacing the VM (new OCID) needs no config change. Setting
``instance_id`` pins one exact instance instead and takes precedence. ``region`` falls back to the
OCI_REGION env var.

Credentials: an OCI API-signing keypair for a dedicated user, in env vars (Fly secrets):
OCI_USER_OCID, OCI_TENANCY_OCID, OCI_FINGERPRINT, OCI_PRIVATE_KEY.

Policy: name mode needs the user to list instances in the compartment and run power actions, which
``use instances in compartment <name>`` grants (``use`` includes read, INSTANCE_POWER_ACTIONS). Pinned
mode can be scoped to one instance (``... where target.instance.id = '<OCID>'``) but then every VM
replacement needs the policy and config updated.
"""

from __future__ import annotations

import os
from functools import lru_cache
from typing import Any

import oci

from .errors import APIError, NotConfigured


def _require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise NotConfigured(f"{name} is not set")
    return value


@lru_cache(maxsize=4)
def _client(region: str) -> "oci.core.ComputeClient":
    config = {
        "user": _require_env("OCI_USER_OCID"),
        "tenancy": _require_env("OCI_TENANCY_OCID"),
        "fingerprint": _require_env("OCI_FINGERPRINT"),
        "key_content": _require_env("OCI_PRIVATE_KEY"),
        "region": region,
    }
    return oci.core.ComputeClient(config)


def _region(node: dict[str, Any]) -> str:
    region = (node.get("oci") or {}).get("region") or os.environ.get("OCI_REGION", "")
    if not region:
        raise NotConfigured('set "oci": {"region": ...} (or the OCI_REGION env var)')
    return region


def _instance_id(node: dict[str, Any]) -> str:
    """The pinned OCID if set, else the live instance named ``instance_name`` in the compartment."""
    block = node.get("oci") or {}
    if block.get("instance_id"):
        return block["instance_id"]
    compartment = block.get("compartment_id")
    if not compartment:
        raise NotConfigured('node config needs "oci": {"compartment_id", "instance_name"} or {"instance_id"}')
    name = block.get("instance_name")
    if not name:
        raise NotConfigured('node config needs "oci": {"instance_name": ...} next to compartment_id')
    try:
        resp = _client(_region(node)).list_instances(compartment_id=compartment, display_name=name)
    except NotConfigured:
        raise
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    live = [i for i in resp.data if i.lifecycle_state not in ("TERMINATING", "TERMINATED")]
    if not live:
        raise APIError(f"no instance named {name!r} in the compartment")
    if len(live) > 1:
        raise APIError(f"{len(live)} live instances named {name!r}; refusing to guess")
    return live[0].id


def status(node: dict[str, Any]) -> dict[str, Any]:
    instance_id = _instance_id(node)
    try:
        resp = _client(_region(node)).get_instance(instance_id)
    except NotConfigured:
        raise
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    return {"status": resp.data.lifecycle_state, "shape": resp.data.shape}


def start(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already RUNNING."""
    instance_id = _instance_id(node)
    previous = status({**node, "oci": {**node["oci"], "instance_id": instance_id}})["status"]
    if previous == "RUNNING":
        return previous, previous
    try:
        _client(_region(node)).instance_action(instance_id, "START")
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    return previous, "STARTING"


def stop(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already STOPPED."""
    instance_id = _instance_id(node)
    previous = status({**node, "oci": {**node["oci"], "instance_id": instance_id}})["status"]
    if previous == "STOPPED":
        return previous, previous
    try:
        _client(_region(node)).instance_action(instance_id, "STOP")
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    return previous, "STOPPING"

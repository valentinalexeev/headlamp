"""Yandex Compute instance start/stop/status.

Node config block: ``"yandex": {"instance_id": "<id>"}``.

Credentials: the service account key JSON in the YANDEX_SA_KEY_JSON env var (a Fly secret). That
account only needs the predefined ``compute.operator`` role (start/stop + read, no create/modify/
delete).
"""

from __future__ import annotations

import json
import os
from functools import lru_cache
from typing import Any

import yandexcloud
from yandex.cloud.compute.v1.instance_pb2 import Instance
from yandex.cloud.compute.v1.instance_service_pb2 import (
    GetInstanceRequest,
    StartInstanceRequest,
    StopInstanceRequest,
)
from yandex.cloud.compute.v1.instance_service_pb2_grpc import InstanceServiceStub

from .errors import APIError, NotConfigured

_STATUS_NAMES = {
    Instance.STATUS_UNSPECIFIED: "UNSPECIFIED",
    Instance.PROVISIONING: "PROVISIONING",
    Instance.RUNNING: "RUNNING",
    Instance.STOPPING: "STOPPING",
    Instance.STOPPED: "STOPPED",
    Instance.RESTARTING: "RESTARTING",
    Instance.UPDATING: "UPDATING",
    Instance.STARTING: "STARTING",
    Instance.ERROR: "ERROR",
    Instance.CRASHED: "CRASHED",
    Instance.DELETING: "DELETING",
}


@lru_cache(maxsize=1)
def _sdk() -> yandexcloud.SDK:
    key_json = os.environ.get("YANDEX_SA_KEY_JSON")
    if not key_json:
        raise NotConfigured("YANDEX_SA_KEY_JSON is not set")
    try:
        key = json.loads(key_json)
    except json.JSONDecodeError as e:
        raise NotConfigured(f"YANDEX_SA_KEY_JSON is not valid JSON: {e}") from e
    return yandexcloud.SDK(service_account_key=key)


def _instance_id(node: dict[str, Any]) -> str:
    instance_id = (node.get("yandex") or {}).get("instance_id")
    if not instance_id:
        raise NotConfigured('node config needs "yandex": {"instance_id": ...}')
    return instance_id


def status(node: dict[str, Any]) -> dict[str, Any]:
    instance_id = _instance_id(node)
    svc = _sdk().client(InstanceServiceStub)
    try:
        instance = svc.Get(GetInstanceRequest(instance_id=instance_id))
    except Exception as e:  # noqa: BLE001 - surfaced as a clean API error to the caller
        raise APIError(str(e)) from e
    return {"status": _STATUS_NAMES.get(instance.status, "UNKNOWN")}


def start(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already RUNNING."""
    instance_id = _instance_id(node)
    previous = status(node)["status"]
    if previous == "RUNNING":
        return previous, previous
    try:
        _sdk().client(InstanceServiceStub).Start(StartInstanceRequest(instance_id=instance_id))
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    return previous, "STARTING"


def stop(node: dict[str, Any]) -> tuple[str, str]:
    """Idempotent: no-ops (returns the same status twice) if already STOPPED."""
    instance_id = _instance_id(node)
    previous = status(node)["status"]
    if previous == "STOPPED":
        return previous, previous
    try:
        _sdk().client(InstanceServiceStub).Stop(StopInstanceRequest(instance_id=instance_id))
    except Exception as e:  # noqa: BLE001
        raise APIError(str(e)) from e
    return previous, "STOPPING"

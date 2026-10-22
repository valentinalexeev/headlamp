"""Node configuration: which nodes exist, how to reach them, and how the UI shows them.

One JSON file (default ``app/nodes.json``, override with the NODES_CONFIG env var) describes every
node. Keep the real file in your private repo and lay it over the placeholder at deploy time. Format:

    {
      "title": "node-control",                      # optional, page title
      "subtitle": "on-demand control for my exit nodes",   # optional
      "nodes": {
        "<id>": {                                   # [a-z0-9_-]; used in the API paths
          "provider": "yandex" | "oci" | "selectel" | "fly",
          "tailnetName": "<headscale given name>",  # default: the id
          "label": "...",                           # UI text, default per provider
          "yandex": {"instance_id": "..."},         # connection block named after the provider
          "oci":    {"region": "...", "compartment_id": "...", "instance_name": "...", "instance_id": "..."},
          "selectel": {"region": "...", "instance_name": "...", "instance_id": "..."},
          "fly":    {"app": "<fly app name>"}
        }
      },
      "tailnetTrack": ["<extra headscale names to report>"]   # optional
    }

Credentials are never in this file: they come from environment variables / Fly secrets (see README.md).
Only the UI-facing keys are ever served to the browser, never the connection blocks.
"""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any

PROVIDERS = ("yandex", "oci", "selectel", "fly")
_ID_RE = re.compile(r"^[a-z0-9][a-z0-9_-]*$")
_UI_KEYS = (
    "startStatus", "stopStatus", "detailFields", "notConfiguredDetailFields", "prettyNotConfigured",
)

# Status strings differ per provider (and are compared uppercased by the UI).
PROVIDER_DEFAULTS: dict[str, dict[str, Any]] = {
    "yandex": {"label": "Yandex Compute", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": ["detail"]},
    "oci": {"label": "OCI Compute", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": ["detail", "shape"]},
    "selectel": {"label": "Selectel Cloud", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": ["detail"]},
    "fly": {
        "label": "Fly Machine", "startStatus": "STARTED", "stopStatus": "STOPPED",
        "detailFields": [{"field": "machine_id", "template": "machine {value}"}],
        "notConfiguredDetailFields": ["detail"], "prettyNotConfigured": True,
    },
}


class ConfigError(Exception):
    """The nodes config is malformed."""


def load(path: str | Path) -> dict[str, Any]:
    try:
        cfg = json.loads(Path(path).read_text())
    except OSError as e:
        raise ConfigError(f"cannot read nodes config {path}: {e}") from e
    except json.JSONDecodeError as e:
        raise ConfigError(f"nodes config {path} is not valid JSON: {e}") from e
    validate(cfg)
    return cfg


def validate(cfg: Any) -> None:
    if not isinstance(cfg, dict) or not isinstance(cfg.get("nodes"), dict):
        raise ConfigError('nodes config must be an object with a "nodes" object')
    for node_id, node in cfg["nodes"].items():
        if not _ID_RE.match(node_id):
            raise ConfigError(f"node id {node_id!r} must match {_ID_RE.pattern}")
        if not isinstance(node, dict) or node.get("provider") not in PROVIDERS:
            raise ConfigError(f"node {node_id!r}: provider must be one of {', '.join(PROVIDERS)}")
        block = node.get(node["provider"])
        if block is not None and not isinstance(block, dict):
            raise ConfigError(f"node {node_id!r}: {node['provider']!r} must be an object")


def ui_view(cfg: dict[str, Any]) -> dict[str, Any]:
    """What the browser may see: labels, status vocabulary, tailnet names. No connection details."""
    nodes = {}
    for node_id, node in cfg["nodes"].items():
        merged = {**PROVIDER_DEFAULTS[node["provider"]], **{k: node[k] for k in (*_UI_KEYS, "label") if k in node}}
        view = {k: merged[k] for k in _UI_KEYS if k in merged}
        view["provider"] = merged["label"]
        view["tailnetName"] = node.get("tailnetName", node_id)
        nodes[node_id] = view
    return {
        "title": cfg.get("title", "node-control"),
        "subtitle": cfg.get("subtitle", "on-demand start/stop for your nodes"),
        "nodes": nodes,
    }


def tailnet_names(cfg: dict[str, Any]) -> set[str]:
    names = {n.get("tailnetName", node_id) for node_id, n in cfg["nodes"].items()}
    return names | set(cfg.get("tailnetTrack", []))

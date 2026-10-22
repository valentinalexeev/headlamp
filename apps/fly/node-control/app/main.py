"""node-control: on-demand start/stop/status for the nodes listed in the nodes config.

Reachable only over the tailnet: this process binds to the tailscale interface's IP (see start.sh),
not 0.0.0.0, and fly.toml has no `[[services]]` block exposing it through Fly's own proxy.
Which nodes exist (and how to reach them) comes from the nodes config, see nodes.py / README.md.
"""

from __future__ import annotations

import importlib
import os
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException
from fastapi.responses import HTMLResponse

from . import headscale, nodes
from .enroll import admin as enroll_admin
from .errors import APIError, NotConfigured

CONFIG = nodes.load(os.environ.get("NODES_CONFIG", str(Path(__file__).parent / "nodes.json")))
_INDEX_HTML = (Path(__file__).parent / "static" / "index.html").read_text()

app = FastAPI(title="node-control", description="start/stop/status for the configured nodes")
enroll_admin.attach_from_env(app)  # /enroll routes only when ENROLL_DB is set


def _provider(node_id: str):
    """(provider module, node config) for a node id; 404 for unknown ids. Provider SDKs import lazily."""
    node = CONFIG["nodes"].get(node_id)
    if node is None:
        raise HTTPException(status_code=404, detail=f"unknown node {node_id!r}")
    return importlib.import_module(f"app.{node['provider']}"), node


def _status(node_id: str) -> dict[str, Any]:
    """A node's status dict; configuration/API problems become a status instead of an exception."""
    mod, node = _provider(node_id)
    try:
        return mod.status(node)
    except NotConfigured as e:
        return {"status": "not_configured", "detail": str(e)}
    except APIError as e:
        return {"status": "error", "detail": str(e)}


def _act(node_id: str, action: str) -> dict[str, Any]:
    mod, node = _provider(node_id)
    try:
        previous, current = getattr(mod, action)(node)
    except NotConfigured as e:
        raise HTTPException(status_code=503, detail=str(e)) from e
    except APIError as e:
        raise HTTPException(status_code=502, detail=str(e)) from e
    return {"node": node_id, "action": action, "previous_status": previous, "current_status": current}


@app.get("/", response_class=HTMLResponse)
def index() -> str:
    return _INDEX_HTML


@app.get("/nodes.json")
def nodes_json() -> dict[str, Any]:
    """What the page renders from: labels, status vocabulary, tailnet names. No connection details."""
    return nodes.ui_view(CONFIG)


@app.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/status")
def status() -> dict[str, Any]:
    return {node_id: _status(node_id) for node_id in CONFIG["nodes"]}


@app.get("/nodes/{node_id}/status")
def node_status(node_id: str) -> dict[str, Any]:
    return _status(node_id)


@app.post("/nodes/{node_id}/start")
def node_start(node_id: str) -> dict[str, Any]:
    return _act(node_id, "start")


@app.post("/nodes/{node_id}/stop")
def node_stop(node_id: str) -> dict[str, Any]:
    return _act(node_id, "stop")


@app.get("/tailnet/status")
def tailnet_status() -> dict[str, Any]:
    """The actual tailnet state of each node as headscale sees it (online/last-seen/routes);
    complements the cloud status, which can't show a healthy VM with broken tailscale."""
    try:
        return headscale.tailnet_status(nodes.tailnet_names(CONFIG))
    except headscale.HeadscaleNotConfigured as e:
        return {"status": "not_configured", "detail": str(e)}
    except headscale.HeadscaleAPIError as e:
        raise HTTPException(status_code=502, detail=str(e)) from e

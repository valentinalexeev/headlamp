"""Operator API and page for enrolment (mounted into node-control only when ENROLL_DB is set).

Tailnet-only, no auth beyond the network, like the other node-control routes. Every POST must
carry ``Content-Type: application/json`` (else 422), which a cross-site HTML form cannot send:
this is the CSRF protection.

Sync visibility: every failed push to the agent is logged (exception class, and the HTTP status for
HTTP failures; never payloads or credentials) and classified as ``unreachable``, ``unauthorized``
(401/403), ``conflict`` (409) or ``rejected`` (other non-2xx, e.g. 503 when xray refused the
config). ``GET /enroll/devices`` returns ``{"devices": [...], "sync": {"pending", "state", "since"}}``
and the create/revoke/extend responses carry ``sync_pending`` and ``sync``; the page shows a banner
while ``pending``. An empty store (no devices at all) is never pushed: the agent would refuse an
unconfirmed empty set with 409 and an empty store must never wipe it, so its state is ``empty``.

Host check: with ``ENROLL_ALLOWED_HOSTS`` (comma-separated) set, requests whose Host header is not
listed get 400 (Starlette TrustedHostMiddleware; it covers the whole admin app), which blocks DNS
rebinding of the admin page from a browser.

Traffic is keyed by an opaque id: the first 8 hex chars of SHA-256 of the device's ``auto`` UUID.
The agent reports stats under that id so the UUID itself never leaves the agent/store.
"""
import asyncio
import contextlib
import hashlib
import json
import logging
import os
import threading
import time
from pathlib import Path

from fastapi import APIRouter, Depends, FastAPI, HTTPException, Request
from fastapi.responses import HTMLResponse
from pydantic import BaseModel, Field
from starlette.middleware.trustedhost import TrustedHostMiddleware

from .agent_client import AgentClient, AgentUnavailable
from .profile import IngressCfg, cfg_from_dict, sub_url
from .store import Store

CLIENTS = ("sing-box", "karing", "clash")
RECONCILE_SECONDS = 300
log = logging.getLogger(__name__)
_SYNC_LOCK = threading.Lock()  # read store + push as one step, so a stale snapshot never lands last
_PAGE = Path(__file__).parent.parent / "static" / "enroll.html"


class SyncState:
    """In-memory sync outcome. ``pending``: the last push failed and has not succeeded since.
    ``state``: unknown (nothing tried yet), ok, empty (nothing to push), unreachable,
    unauthorized, rejected, conflict, error (unexpected exception). ``since``: unix time of the
    first failure of the current pending stretch, else None."""

    def __init__(self, clock=lambda: time.time()):
        self.clock = clock
        self.pending = False
        self.state = "unknown"
        self.since = None

    def ok(self, state: str = "ok") -> None:
        self.pending, self.state, self.since = False, state, None

    def failed(self, state: str) -> None:
        if not self.pending:
            self.since = int(self.clock())
        self.pending, self.state = True, state

    def snapshot(self) -> dict:
        return {"pending": self.pending, "state": self.state, "since": self.since}


def sync(store: Store, agent, state: "SyncState | None" = None) -> bool:
    """Push the live credentials. An empty set is confirmed only when the store has devices
    (all revoked/expired = deliberate); an empty store is not pushed at all (never wipes the agent)."""
    state = state if state is not None else SyncState()
    with _SYNC_LOCK:
        creds = store.credentials()
        has_devices = bool(store.list_devices())
        if not creds and not has_devices:
            state.ok("empty")
            return True
        try:
            agent.put_credentials(creds, confirm_empty=not creds)
        except AgentUnavailable as e:
            outcome = getattr(e, "outcome", "unreachable")
            status = getattr(e, "status", None)
            log.warning("enrolment sync failed: %s (%s%s)", outcome, type(e).__name__,
                        f", status {status}" if status is not None else "")
            state.failed(outcome)
            return False
        state.ok()
        return True


def reconcile(store: Store, agent, state: "SyncState | None" = None) -> bool:
    return sync(store, agent, state)


async def reconcile_loop(store, agent, state) -> None:
    while True:
        await asyncio.sleep(RECONCILE_SECONDS)
        try:
            await asyncio.to_thread(reconcile, store, agent, state)
        except Exception as e:  # noqa: BLE001 - keep the loop alive; log the class only
            log.error("enrolment reconcile failed: %s", type(e).__name__)
            state.failed("error")


def opaque_id(device) -> str:
    return hashlib.sha256(device.uuids["auto"].encode()).hexdigest()[:8]


def _require_json(request: Request) -> None:
    if request.headers.get("content-type", "").split(";")[0].strip().lower() != "application/json":
        raise HTTPException(status_code=422, detail="Content-Type must be application/json")


class DeviceIn(BaseModel):
    name: str = Field(min_length=1, max_length=100)
    owner: str = Field(min_length=1, max_length=100)
    client: str
    expires_at: int


class LinkIn(BaseModel):
    ttl: int = Field(ge=60, le=604800)
    max_uses: int = Field(ge=1, le=100)


class ExtendIn(BaseModel):
    expires_at: int


def create_router(store: Store, cfg: IngressCfg, agent) -> APIRouter:
    router = APIRouter()
    state = SyncState(clock=store.clock)
    router.sync_state = state
    page = _PAGE.read_text()

    def future(ts: int) -> int:
        if ts <= store.clock():
            raise HTTPException(status_code=422, detail="expires_at must be in the future")
        return ts

    def known(device_id: int) -> None:
        if not any(d.id == device_id for d in store.list_devices()):
            raise HTTPException(status_code=404, detail="unknown device")

    @router.get("/enroll", response_class=HTMLResponse)
    def enroll_page() -> str:
        return page

    @router.get("/enroll/devices")
    def list_devices() -> dict:
        stats = agent.get_stats() or {}
        devices = [
            {
                "id": d.id,
                "name": d.name,
                "owner": d.owner,
                "client": d.client,
                "expires_at": d.expires_at,
                "revoked": d.revoked,
                "traffic": stats.get(opaque_id(d)),
            }
            for d in store.list_devices()
        ]
        return {"devices": devices, "sync": state.snapshot()}

    @router.post("/enroll/devices", dependencies=[Depends(_require_json)])
    def create_device(body: DeviceIn) -> dict:
        if body.client not in CLIENTS:
            raise HTTPException(status_code=422, detail=f"client must be one of {CLIENTS}")
        future(body.expires_at)
        device, secret = store.create_device(body.name, body.owner, body.client, body.expires_at)
        sync(store, agent, state)
        return {"id": device.id, "subscription_url": sub_url(cfg, secret), "sync_pending": state.pending,
                "sync": state.snapshot()}

    @router.post("/enroll/devices/{device_id}/links", dependencies=[Depends(_require_json)])
    def new_link(device_id: int, body: LinkIn) -> dict:
        known(device_id)
        token = store.create_token(device_id, ttl=body.ttl, max_uses=body.max_uses)
        return {"url": f"https://{cfg.host}/e/{token}"}

    @router.post("/enroll/devices/{device_id}/revoke", dependencies=[Depends(_require_json)])
    def revoke(device_id: int) -> dict:
        known(device_id)
        store.revoke_device(device_id)
        sync(store, agent, state)
        return {"ok": True, "sync_pending": state.pending, "sync": state.snapshot()}

    @router.post("/enroll/devices/{device_id}/extend", dependencies=[Depends(_require_json)])
    def extend(device_id: int, body: ExtendIn) -> dict:
        known(device_id)
        future(body.expires_at)
        store.extend_device(device_id, body.expires_at)
        sync(store, agent, state)
        return {"ok": True, "sync_pending": state.pending, "sync": state.snapshot()}

    @router.post("/enroll/devices/{device_id}/rotate-secret", dependencies=[Depends(_require_json)])
    def rotate(device_id: int) -> dict:
        known(device_id)
        store.rotate_secret(device_id)
        return {"subscription_url": sub_url(cfg, store.subscription_secret(device_id))}

    return router


def attach_from_env(app: FastAPI) -> bool:
    """Mount the router and the 5-minute reconcile loop when ENROLL_DB is set."""
    if not os.environ.get("ENROLL_DB"):
        return False
    missing = [k for k in ("ENROLL_CONFIG", "ENROLL_SECRET_KEY", "AGENT_URL", "AGENT_TOKEN") if not os.environ.get(k)]
    if missing:
        raise RuntimeError(f"enrolment enabled (ENROLL_DB set) but missing: {', '.join(missing)}")
    with open(os.environ["ENROLL_CONFIG"]) as f:
        cfg = cfg_from_dict(json.load(f))
    key = os.environ["ENROLL_SECRET_KEY"]
    store = Store(os.environ["ENROLL_DB"], key.encode())
    agent = AgentClient(os.environ["AGENT_URL"], os.environ["AGENT_TOKEN"])
    router = create_router(store, cfg, agent)
    app.include_router(router)
    hosts = [h.strip() for h in os.environ.get("ENROLL_ALLOWED_HOSTS", "").split(",") if h.strip()]
    if hosts:
        app.add_middleware(TrustedHostMiddleware, allowed_hosts=hosts)
    inner = app.router.lifespan_context

    @contextlib.asynccontextmanager
    async def lifespan(a):
        task = asyncio.create_task(reconcile_loop(store, agent, router.sync_state))
        try:
            async with inner(a) as state:
                yield state
        finally:
            task.cancel()

    app.router.lifespan_context = lifespan
    return True

"""Ingress agent: holds opaque credentials, owns the xray config, nothing else.

Never logs uuids, emails or tokens; only counts.
"""
import asyncio
import contextlib
import hashlib
import hmac
import json
import logging
import os
import shutil
import tempfile
import threading
import time
import uuid as uuidlib
from pathlib import Path
from typing import Callable, Literal

from fastapi import Depends, FastAPI, Header, HTTPException, Response
from pydantic import BaseModel, StrictInt, field_validator, model_validator
from starlette.exceptions import HTTPException as StarletteHTTPException

from .store import Cred, diff, load, save, sweep
from .xray import XrayRunner, email_for, parse_list, render_config

log = logging.getLogger("ingress_agent")
SWEEP_SECONDS = 60


class ApplyError(Exception):
    """The new config did not validate or could not be installed; nothing changed."""


class Applier:
    def __init__(self, state_path, base_path, config_path, runner: XrayRunner, clock: Callable[[], int] = lambda: int(time.time()),
                 direct_geoip=(), direct_geosite=()):
        self.state_path, self.base_path, self.config_path = Path(state_path), Path(base_path), Path(config_path)
        self.runner, self.clock = runner, clock
        self.direct_geoip, self.direct_geosite = tuple(direct_geoip), tuple(direct_geosite)
        self._lock = threading.Lock()

    def current(self) -> list[Cred]:
        return load(self.state_path)

    def _render(self, creds: list[Cred]) -> dict:
        return render_config(json.loads(self.base_path.read_text()), creds, self.direct_geoip, self.direct_geosite)

    def _install(self, cfg: dict) -> None:
        """Write next to the live config, validate, then atomically replace. Raises ApplyError."""
        # xray picks the config format from the file extension, so the validated temp file must end in .json.
        fd, tmp = tempfile.mkstemp(dir=self.config_path.parent, prefix=f".{self.config_path.name}.", suffix=".json")
        try:
            with os.fdopen(fd, "w") as f:
                json.dump(cfg, f)
            with contextlib.suppress(OSError):
                shutil.copymode(self.base_path, tmp)
            if not self.runner.test_config(tmp):
                raise ApplyError("xray rejected the generated config")
            os.replace(tmp, self.config_path)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)

    def apply(self, desired: list[Cred]) -> bool:
        with self._lock:
            return self._apply_locked(desired)

    def _apply_locked(self, desired: list[Cred]) -> bool:
        """Returns True when something changed. The set is persisted only after xray restarted."""
        desired = sweep(desired, self.clock())
        added, removed = diff(self.current(), desired)
        if not added and not removed:
            return False
        try:
            self._install(self._render(desired))
            self.runner.restart()
        except ApplyError:
            raise
        except Exception as e:  # noqa: BLE001
            raise ApplyError(type(e).__name__) from e
        save(self.state_path, desired)
        log.info("applied: +%d -%d", len(added), len(removed))
        return True

    def expire(self) -> bool:
        with self._lock:  # sweep and apply as one step, so a concurrent PUT is never reverted
            cur = self.current()
            alive = sweep(cur, self.clock())
            return self._apply_locked(alive) if len(alive) != len(cur) else False

    def restore(self) -> None:
        """Re-render the persisted set after a reboot; restart only if the file on disk differs."""
        with self._lock:
            creds = sweep(self.current(), self.clock())
            cfg = self._render(creds)
            try:
                if json.loads(self.config_path.read_text()) == cfg:
                    return
            except (OSError, ValueError):
                pass
            self._install(cfg)
            self.runner.restart()
            save(self.state_path, creds)
            log.info("restored: %d", len(creds))


class CredIn(BaseModel):
    uuid: str
    mode: Literal["auto", "direct", "exit"]
    not_after: StrictInt

    @field_validator("uuid")
    @classmethod
    def _uuid(cls, v: str) -> str:
        try:
            return str(uuidlib.UUID(v))
        except ValueError as e:
            raise ValueError("not a uuid") from e


class CredsIn(BaseModel):
    credentials: list[CredIn]

    @model_validator(mode="after")
    def _unique(self):
        ids = [c.uuid for c in self.credentials]
        if len(ids) != len(set(ids)):
            raise ValueError("duplicate uuid")
        return self


def opaque_id(uuid: str) -> str:
    return hashlib.sha256(uuid.encode()).hexdigest()[:8]


async def sweep_loop(applier, interval: float, sleep=asyncio.sleep) -> None:
    while True:
        await sleep(interval)
        try:
            await asyncio.to_thread(applier.expire)
        except Exception as e:  # noqa: BLE001 - keep sweeping; class name only
            log.error("expiry sweep failed: %s", type(e).__name__)


def create_app(applier: Applier, token: str, clock: Callable[[], int] = lambda: int(time.time()), sweep_interval: float = SWEEP_SECONDS) -> FastAPI:
    @contextlib.asynccontextmanager
    async def lifespan(_app):
        try:
            await asyncio.to_thread(applier.restore)
        except Exception as e:  # noqa: BLE001 - serve anyway; the next push repairs
            log.error("startup restore failed: %s", type(e).__name__)
        task = asyncio.create_task(sweep_loop(applier, sweep_interval))
        try:
            yield
        finally:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task

    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None, lifespan=lifespan)

    @app.exception_handler(StarletteHTTPException)
    async def _plain(_req, exc):
        return Response(status_code=exc.status_code)  # no body, no detail

    def auth(authorization: str | None = Header(default=None)) -> None:
        expected = f"Bearer {token}"
        if authorization is None or not hmac.compare_digest(authorization.encode(), expected.encode()):
            raise HTTPException(status_code=401)

    @app.put("/v1/credentials", dependencies=[Depends(auth)])
    def put_credentials(payload: CredsIn, confirm_empty: bool = False) -> dict:
        creds = sweep([Cred(c.uuid, c.mode, c.not_after) for c in payload.credentials], clock())
        if not creds and not confirm_empty:
            raise HTTPException(status_code=409)
        try:
            applier.apply(creds)
        except ApplyError as e:
            log.error("apply failed: %s", e)
            raise HTTPException(status_code=503) from e
        return {"count": len(creds)}

    @app.get("/v1/stats", dependencies=[Depends(auth)])
    def stats() -> dict:
        by_email = applier.runner.stats()
        out = {}
        for c in applier.current():
            s = by_email.get(email_for(c.uuid))
            if s is not None:
                out[opaque_id(c.uuid)] = {"up": s["up"], "down": s["down"]}
        return out

    return app


def create_app_from_env() -> FastAPI:
    env = os.environ
    token = env.get("AGENT_TOKEN")
    if not token:
        raise RuntimeError("AGENT_TOKEN is required")
    state_dir = Path(env.get("AGENT_STATE_DIR", "/var/lib/ingress-agent"))
    runner = XrayRunner(env.get("XRAY_BIN", "/usr/local/bin/xray"), env.get("XRAY_UNIT", "xray"), env.get("XRAY_API", "127.0.0.1:10085"))
    applier = Applier(state_dir / "credentials.json", env.get("XRAY_BASE_CONFIG", "/etc/xray/config.base.json"),
                      env.get("XRAY_CONFIG", "/etc/xray/config.json"), runner,
                      direct_geoip=parse_list(env.get("INGRESS_DIRECT_GEOIP", "")),
                      direct_geosite=parse_list(env.get("INGRESS_DIRECT_GEOSITE", "")))
    return create_app(applier, token)

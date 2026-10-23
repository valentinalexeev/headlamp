"""Public enrolment listener: serves ONLY ``/e/{token}`` and ``/sub/{secret}``.

Meant to run on a private address (e.g. a tailnet IP) behind a reverse proxy, so it must expose
nothing else: no docs/openapi, no health route, and every other path or method answers an empty
404, byte-identical to an unknown or used token.

Start it with uvicorn's access log OFF (``--no-access-log``) and never log request paths: they
carry one-time tokens and subscription secrets. This module logs nothing itself.

    ENROLL_DB=/var/lib/tailscale/enroll.db ENROLL_SECRET_KEY=... ENROLL_CONFIG=/srv/app/enroll.json \\
        uvicorn --factory app.enroll.public:create_app_from_env --no-access-log

``ENROLL_SECRET_KEY`` (>= 16 bytes) derives subscription secrets. ``ENROLL_CONFIG`` is a JSON file
with ``host``, ``public_key``, ``short_id``, ``sni`` and optional ``port`` (443) and ``profile_name``
(``"tunnel"``).

Clients whose app only opens from a tap (``karing``) get a small HTML page with an "Open in ..." button
instead of the 302; the others get the 302.

Privacy: the redirect (or that page) and the profile pass through the ingress host, so neither carries the device
name or owner; the deep link label is the neutral ``profile_name``.

Link previews: messengers fetch a pasted URL to render a preview, which would consume a single-use
``/e/`` link before the person taps it. Requests whose User-Agent contains (case-insensitive) one
of ``PREVIEW_UA_MARKERS`` get the same empty 404 as every other 404 and do NOT consume the token;
``/sub/`` answers them the same way. An empty or missing User-Agent is a normal client. A preview
bot with an unlisted User-Agent can still burn a link: create another one.
"""
import html
import json
import os

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from .profile import IngressCfg, cfg_from_dict, deep_link, render_profile, sub_url
from .store import Store

# Case-insensitive substrings of link-preview / crawler User-Agents. iMessage's preview fetch
# identifies as facebookexternalhit/Twitterbot. "apple" is deliberately absent (real Safari).
PREVIEW_UA_MARKERS = (
    "facebookexternalhit", "twitterbot", "telegrambot", "whatsapp", "slackbot", "discordbot",
    "skypeuripreview", "linkedinbot", "embedly", "bot", "crawler", "spider", "preview",
    "linkexpanding", "google-",
)


def is_preview(user_agent: "str | None") -> bool:
    ua = (user_agent or "").lower()
    return any(m in ua for m in PREVIEW_UA_MARKERS)


_LANDING = """<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Open in {client}</title>
<style>body{{font:18px system-ui,sans-serif;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;text-align:center}}
a{{display:inline-block;margin-top:1.2em;padding:.9em 1.6em;border-radius:.7em;background:#0a66ff;color:#fff;text-decoration:none;font-weight:600}}</style>
</head><body><main><p>Tap the button to add the profile in {client}.</p><a href="{href}">Open in {client}</a></main></body></html>
"""


def _landing(client: str, location: str) -> Response:
    """A page with an explicit button: some iOS apps only open from a tap, not from a 302 to their URL scheme."""
    label = {"karing": "Karing"}.get(client, client)
    body = _LANDING.format(client=html.escape(label), href=html.escape(location, quote=True))
    return Response(body, media_type="text/html; charset=utf-8",
                    headers={"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})


def _not_found() -> Response:
    return Response(status_code=404)


def create_app(store: Store, cfg: IngressCfg) -> FastAPI:
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    app.router.redirect_slashes = False  # a 307 would differ from every other 404

    @app.exception_handler(StarletteHTTPException)
    async def _any_http_error(request, exc):  # unknown path, wrong method (405), ...
        return _not_found()

    @app.get("/e/{token}")
    def enrol(token: str, request: Request):
        if is_preview(request.headers.get("user-agent")):
            return _not_found()  # before redeem: a preview must not consume the token
        device = store.redeem_token(token)
        if device is None:
            return _not_found()
        location = deep_link(device.client, sub_url(cfg, store.subscription_secret(device.id)), cfg.profile_name)
        if device.client == "karing":
            return _landing(device.client, location)
        return Response(status_code=302, headers={"Location": location})

    @app.get("/sub/{secret}")
    def subscription(secret: str, request: Request):
        if is_preview(request.headers.get("user-agent")):
            return _not_found()
        device = store.device_by_secret(secret)
        if device is None:
            return _not_found()
        return JSONResponse(render_profile(device, cfg), headers={"Profile-Update-Interval": "12"})

    return app


def create_app_from_env() -> FastAPI:
    """Factory for ``uvicorn --factory``; reads ENROLL_DB and ENROLL_CONFIG."""
    with open(os.environ["ENROLL_CONFIG"]) as f:
        cfg = cfg_from_dict(json.load(f))

    key = os.environ.get("ENROLL_SECRET_KEY")
    if not key:
        raise RuntimeError("ENROLL_SECRET_KEY is required (>= 16 bytes; derives subscription secrets)")
    return create_app(Store(os.environ["ENROLL_DB"], key.encode()), cfg)

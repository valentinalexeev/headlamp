"""Public parameters of this ingress, served at GET /v1/info.

Only public values are read here. There is deliberately no variable for the Reality private
key (or any credential): an operator approves these values in node-control, so they must be
safe to show. Invalid values stop the start-up with an error that names the variable, never
its value.
"""
import re
from typing import Mapping

NAME_RE = re.compile(r"^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?\Z")
LABEL_RE = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\Z")
REQUIRED = ("INGRESS_NAME", "INGRESS_HOST", "INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID")
DEFAULT_PORT = 443


class InfoConfigError(RuntimeError):
    """An INGRESS_* variable is invalid. The message names the variable only."""

    def __init__(self, var: str):
        super().__init__(f"{var} is invalid")
        self.var = var


def _dns_name(v: str) -> bool:
    if not v.isascii() or len(v) > 253:
        return False
    return all(LABEL_RE.match(label) for label in v.split("."))


def _token(v: str) -> bool:
    return bool(v) and not any(ch.isspace() for ch in v)


def load_info(env: Mapping[str, str]) -> dict | None:
    """The /v1/info body, or None when the deployment does not configure it (answered with 503)."""
    vals = {k: env.get(k, "") for k in (*REQUIRED, "INGRESS_PORT", "INGRESS_SNI", "INGRESS_HY2_PORT")}
    # Validate whatever is set, even if the set is incomplete: a typo must not hide until later.
    if vals["INGRESS_NAME"] and not NAME_RE.match(vals["INGRESS_NAME"]):
        raise InfoConfigError("INGRESS_NAME")
    for var in ("INGRESS_HOST", "INGRESS_SNI"):
        if vals[var] and not _dns_name(vals[var]):
            raise InfoConfigError(var)
    port = DEFAULT_PORT
    if vals["INGRESS_PORT"]:
        port = _port(vals["INGRESS_PORT"], "INGRESS_PORT")
    # INGRESS_HY2_PORT: the public UDP port of the Hysteria 2 inbound (#110), if this bridge has one. The Salamander
    # password is not here: it is a secret, and node-control gets it from its own environment.
    hy2 = _port(vals["INGRESS_HY2_PORT"], "INGRESS_HY2_PORT") if vals["INGRESS_HY2_PORT"] else None
    for var in ("INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID"):
        if env.get(var, "") and not _token(vals[var]):
            raise InfoConfigError(var)
    if not all(vals[k] for k in REQUIRED):
        return None
    out = {"name": vals["INGRESS_NAME"], "host": vals["INGRESS_HOST"], "port": port,
           "sni": vals["INGRESS_SNI"] or vals["INGRESS_HOST"],
           "public_key": vals["INGRESS_PUBLIC_KEY"], "short_id": vals["INGRESS_SHORT_ID"]}
    if hy2 is not None:
        out["hy2_port"] = hy2
    return out


def _port(raw: str, var: str) -> int:
    if not (raw.isascii() and raw.isdigit()) or not 1 <= int(raw) <= 65535:
        raise InfoConfigError(var)
    return int(raw)

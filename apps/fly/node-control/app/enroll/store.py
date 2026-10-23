"""SQLite store for enrolment: devices, per-mode UUIDs, expiring tokens.

Tokens are 128-bit random values; only their SHA-256 hex digest is stored. Subscription secrets
are not stored at all: they are derived as ``<device id>.<HMAC-SHA256(secret_key, sub:id:epoch)>``
and verified by recomputation. ``rotate_secret`` bumps the epoch, invalidating the old secret.
"""
import base64
import hashlib
import hmac
import secrets
import sqlite3
import time
import uuid
from contextlib import closing
from dataclasses import dataclass, field
from typing import Callable, Literal

# Routing modes, one UUID each per device. auto: split by the ingress agent's configured direct lists;
# direct: leave from the ingress host itself; exit: leave through the exit node.
MODES = ("auto", "direct", "exit")

_SCHEMA = """
CREATE TABLE IF NOT EXISTS devices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    owner TEXT NOT NULL,
    client TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0,
    secret_epoch INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS device_uuids (
    device_id INTEGER NOT NULL REFERENCES devices(id),
    mode TEXT NOT NULL,
    uuid TEXT NOT NULL,
    PRIMARY KEY (device_id, mode)
);
CREATE TABLE IF NOT EXISTS tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id INTEGER NOT NULL REFERENCES devices(id),
    token_hash TEXT NOT NULL UNIQUE,
    expires INTEGER NOT NULL,
    max_uses INTEGER NOT NULL,
    uses INTEGER NOT NULL DEFAULT 0
);
"""


def _hash(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


@dataclass(frozen=True)
class Device:
    id: int
    name: str
    owner: str
    client: str
    expires_at: int
    revoked: bool
    uuids: dict = field(default_factory=dict)  # mode -> uuid string


class Store:
    def __init__(self, path: str, secret_key: bytes, clock: Callable[[], float] = time.time):
        if not isinstance(secret_key, (bytes, bytearray)) or len(secret_key) < 16:
            raise ValueError("secret_key must be at least 16 bytes")
        self.path = path
        self._key = bytes(secret_key)
        self.clock = clock
        with closing(self._connect()) as db:
            db.execute("PRAGMA journal_mode=WAL")
            db.executescript(_SCHEMA)
            cols = {r["name"] for r in db.execute("PRAGMA table_info(devices)")}
            if "secret_epoch" not in cols:
                db.execute("ALTER TABLE devices ADD COLUMN secret_epoch INTEGER NOT NULL DEFAULT 0")
            # Databases created before secrets were derived keep a NOT NULL secret_hash column;
            # it is ignored, and filled with a random placeholder on insert.
            self._legacy_hash = "secret_hash" in cols

    def _mac(self, device_id: int, epoch: int) -> str:
        digest = hmac.new(self._key, f"sub:{device_id}:{epoch}".encode(), hashlib.sha256).digest()
        return base64.urlsafe_b64encode(digest[:16]).rstrip(b"=").decode()

    def subscription_secret(self, device_id: int) -> str:
        with closing(self._connect()) as db:
            row = db.execute(
                "SELECT secret_epoch FROM devices WHERE id=?", (device_id,)
            ).fetchone()
        if row is None:
            raise KeyError(device_id)
        return f"{device_id}.{self._mac(device_id, row['secret_epoch'])}"

    def rotate_secret(self, device_id: int) -> None:
        with closing(self._connect()) as db:
            db.execute(
                "UPDATE devices SET secret_epoch = secret_epoch + 1 WHERE id=?", (device_id,)
            )

    def _connect(self) -> sqlite3.Connection:
        # One connection per call: safe across threads.
        db = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA foreign_keys=ON")
        return db

    @staticmethod
    def _device(db: sqlite3.Connection, row: sqlite3.Row) -> Device:
        uuids = {
            r["mode"]: r["uuid"]
            for r in db.execute(
                "SELECT mode, uuid FROM device_uuids WHERE device_id=?", (row["id"],)
            )
        }
        return Device(
            id=row["id"],
            name=row["name"],
            owner=row["owner"],
            client=row["client"],
            expires_at=row["expires_at"],
            revoked=bool(row["revoked"]),
            uuids=uuids,
        )

    def create_device(
        self,
        name: str,
        owner: str,
        client: Literal["sing-box", "karing", "clash"],
        expires_at: int,
    ) -> tuple[Device, str]:
        with closing(self._connect()) as db:
            db.execute("BEGIN IMMEDIATE")
            try:
                if self._legacy_hash:
                    cur = db.execute(
                        "INSERT INTO devices (name, owner, client, expires_at, secret_hash)"
                        " VALUES (?,?,?,?,?)",
                        (name, owner, client, int(expires_at), secrets.token_hex(16)),
                    )
                else:
                    cur = db.execute(
                        "INSERT INTO devices (name, owner, client, expires_at) VALUES (?,?,?,?)",
                        (name, owner, client, int(expires_at)),
                    )
                dev_id = cur.lastrowid
                db.executemany(
                    "INSERT INTO device_uuids (device_id, mode, uuid) VALUES (?,?,?)",
                    [(dev_id, m, str(uuid.uuid4())) for m in MODES],
                )
                db.execute("COMMIT")
            except BaseException:
                db.execute("ROLLBACK")
                raise
            row = db.execute("SELECT * FROM devices WHERE id=?", (dev_id,)).fetchone()
            device = self._device(db, row)
        return device, self.subscription_secret(device.id)

    def create_token(self, device_id: int, ttl: int = 86400, max_uses: int = 1) -> str:
        token = secrets.token_urlsafe(16)
        with closing(self._connect()) as db:
            db.execute(
                "INSERT INTO tokens (device_id, token_hash, expires, max_uses)"
                " VALUES (?,?,?,?)",
                (device_id, _hash(token), int(self.clock() + ttl), max_uses),
            )
        return token

    def redeem_token(self, token: str) -> "Device | None":
        now = self.clock()
        h = _hash(token)
        with closing(self._connect()) as db:
            cur = db.execute(
                "UPDATE tokens SET uses = uses + 1"
                " WHERE token_hash=? AND uses < max_uses AND expires > ?"
                " AND device_id IN (SELECT id FROM devices"
                "                   WHERE revoked=0 AND expires_at > ?)",
                (h, now, now),
            )
            if cur.rowcount != 1:
                return None
            row = db.execute(
                "SELECT d.* FROM devices d JOIN tokens t ON t.device_id=d.id"
                " WHERE t.token_hash=?",
                (h,),
            ).fetchone()
            return self._device(db, row)

    def device_by_secret(self, secret: str) -> "Device | None":
        dev_id, sep, mac = secret.partition(".")
        if not sep or not dev_id.isascii() or not dev_id.isdigit() or len(dev_id) > 12:
            return None
        with closing(self._connect()) as db:
            row = db.execute(
                "SELECT * FROM devices WHERE id=? AND revoked=0 AND expires_at > ?",
                (int(dev_id), self.clock()),
            ).fetchone()
            if row is None:
                return None
            expected = self._mac(row["id"], row["secret_epoch"])
            if not hmac.compare_digest(mac.encode(), expected.encode()):
                return None
            return self._device(db, row)

    def revoke_device(self, device_id: int) -> None:
        with closing(self._connect()) as db:
            db.execute("UPDATE devices SET revoked=1 WHERE id=?", (device_id,))

    def extend_device(self, device_id: int, expires_at: int) -> None:
        with closing(self._connect()) as db:
            db.execute(
                "UPDATE devices SET expires_at=? WHERE id=?", (int(expires_at), device_id)
            )

    def list_devices(self) -> list:
        with closing(self._connect()) as db:
            rows = db.execute("SELECT * FROM devices ORDER BY id").fetchall()
            return [self._device(db, r) for r in rows]

    def credentials(self) -> list:
        with closing(self._connect()) as db:
            rows = db.execute(
                "SELECT u.uuid, u.mode, d.expires_at AS not_after"
                " FROM devices d JOIN device_uuids u ON u.device_id=d.id"
                " WHERE d.revoked=0 AND d.expires_at > ? ORDER BY d.id, u.mode",
                (self.clock(),),
            ).fetchall()
            return [dict(r) for r in rows]

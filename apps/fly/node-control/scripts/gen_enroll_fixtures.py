#!/usr/bin/env python3
"""Generate the enrolment store test fixtures with the Python Store (needs python3, stdlib only).

Usage: gen_enroll_fixtures.py <python node-control dir> <output testdata dir>
All names, owners and the key below are fake.
"""
import json
import os
import shutil
import sqlite3
import sys
import tempfile

src, out = sys.argv[1], sys.argv[2]
sys.path.insert(0, src)
from app.enroll.store import Store  # noqa: E402

KEY = b"fixture-key-0123456789abcdef"
NOW = 1_000_000.0
clock = lambda: NOW  # noqa: E731


def dump(store):
    devs = []
    for d in store.list_devices():
        devs.append({"id": d.id, "name": d.name, "owner": d.owner, "client": d.client,
                     "expires_at": d.expires_at, "revoked": d.revoked, "uuids": d.uuids,
                     "secret": store.subscription_secret(d.id)})
    return {"devices": devs, "credentials": store.credentials()}


def finish(path):
    db = sqlite3.connect(path)
    db.execute("PRAGMA journal_mode=DELETE")
    db.close()


os.makedirs(out, exist_ok=True)
modern = os.path.join(out, "python_enroll.db")
legacy = os.path.join(out, "python_enroll_legacy.db")
for p in (modern, legacy):
    if os.path.exists(p):
        os.remove(p)

s = Store(modern, KEY, clock=clock)
a, _ = s.create_device("device-a", "owner-a", "sing-box", 2_000_000)
s.create_device("device-b", "owner-b", "clash", 2_000_000)
c, _ = s.create_device("device-c", "owner-c", "karing", 999_000)
s.revoke_device(2)
s.rotate_secret(a.id)
token = s.create_token(a.id, ttl=86400, max_uses=2)
expected = {"key": KEY.decode(), "now": int(NOW), "modern": dump(s), "token_for_device_a": token,
            "token_max_uses": 2}
finish(modern)

db = sqlite3.connect(legacy)
db.executescript(
    "CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,"
    " owner TEXT NOT NULL, client TEXT NOT NULL, expires_at INTEGER NOT NULL,"
    " revoked INTEGER NOT NULL DEFAULT 0, secret_hash TEXT NOT NULL UNIQUE);"
    "CREATE TABLE device_uuids (device_id INTEGER NOT NULL REFERENCES devices(id),"
    " mode TEXT NOT NULL, uuid TEXT NOT NULL, PRIMARY KEY (device_id, mode));"
    "INSERT INTO devices (name, owner, client, expires_at, secret_hash)"
    " VALUES ('device-l1','owner-l1','sing-box',2000000,'legacy-hash-1');"
    "INSERT INTO devices (name, owner, client, expires_at, revoked, secret_hash)"
    " VALUES ('device-l2','owner-l2','clash',2000000,1,'legacy-hash-2');"
    "INSERT INTO device_uuids VALUES (1,'auto','00000000-0000-4000-8000-000000000001');"
    "INSERT INTO device_uuids VALUES (1,'direct','00000000-0000-4000-8000-000000000002');"
    "INSERT INTO device_uuids VALUES (1,'exit','00000000-0000-4000-8000-000000000003');"
)
db.commit()
db.close()
# Expected values come from Python opening a throwaway copy (migration must not touch the shipped file).
with tempfile.TemporaryDirectory() as tmp:
    cp = os.path.join(tmp, "l.db")
    shutil.copy(legacy, cp)
    expected["legacy"] = dump(Store(cp, KEY, clock=clock))

with open(os.path.join(out, "python_expected.json"), "w") as f:
    json.dump(expected, f, indent=2, sort_keys=True)
    f.write("\n")

import os
import sqlite3
import tempfile
import threading
import unittest

import _helpers  # noqa: F401
from app.enroll.store import Device, Store

MODES = {"auto", "direct", "exit"}
KEY = b"test-key-0123456789abcdef"


class Clock:
    def __init__(self, now=1_000_000.0):
        self.now = now

    def __call__(self):
        return self.now


class StoreTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = os.path.join(self.tmp.name, "enroll.db")
        self.clock = Clock()
        self.store = Store(self.path, KEY, clock=self.clock)

    def make(self, name="phone", expires_at=None):
        if expires_at is None:
            expires_at = int(self.clock.now) + 3600
        return self.store.create_device(name, "alice", "sing-box", expires_at)

    def test_create_device(self):
        dev, secret = self.make()
        self.assertIsInstance(dev, Device)
        self.assertEqual(set(dev.uuids), MODES)
        self.assertEqual(len(set(dev.uuids.values())), 3)
        self.assertFalse(dev.revoked)
        self.assertEqual(dev.owner, "alice")
        self.assertEqual(dev.client, "sing-box")
        self.assertTrue(len(secret) >= 22)
        self.assertEqual(secret, self.store.subscription_secret(dev.id))
        self.assertTrue(secret.startswith(f"{dev.id}."))
        self.assertEqual(self.store.device_by_secret(secret), dev)

    def test_second_redeem_of_single_use_token_is_none(self):
        dev, _ = self.make()
        tok = self.store.create_token(dev.id)
        self.assertEqual(self.store.redeem_token(tok), dev)
        self.assertIsNone(self.store.redeem_token(tok))

    def test_multi_use_token(self):
        dev, _ = self.make()
        tok = self.store.create_token(dev.id, max_uses=2)
        self.assertIsNotNone(self.store.redeem_token(tok))
        self.assertIsNotNone(self.store.redeem_token(tok))
        self.assertIsNone(self.store.redeem_token(tok))

    def test_token_past_ttl_is_none(self):
        dev, _ = self.make()
        tok = self.store.create_token(dev.id, ttl=60)
        self.clock.now += 61
        self.assertIsNone(self.store.redeem_token(tok))

    def test_unknown_token_is_none(self):
        self.assertIsNone(self.store.redeem_token("nope"))

    def test_token_of_revoked_or_expired_device_is_none(self):
        dev, _ = self.make()
        tok = self.store.create_token(dev.id)
        self.store.revoke_device(dev.id)
        self.assertIsNone(self.store.redeem_token(tok))
        dev2, _ = self.make("b", expires_at=int(self.clock.now) + 10)
        tok2 = self.store.create_token(dev2.id, ttl=1000)
        self.clock.now += 11
        self.assertIsNone(self.store.redeem_token(tok2))

    def test_concurrent_redeem_exactly_one_wins(self):
        dev, _ = self.make()
        tok = self.store.create_token(dev.id)
        results = []
        barrier = threading.Barrier(8)

        def go():
            barrier.wait()
            results.append(self.store.redeem_token(tok))

        threads = [threading.Thread(target=go) for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(len([r for r in results if r is not None]), 1)

    def test_secret_after_revoke_is_none(self):
        dev, secret = self.make()
        self.store.revoke_device(dev.id)
        self.assertIsNone(self.store.device_by_secret(secret))
        self.assertTrue(self.store.list_devices()[0].revoked)

    def test_secret_after_expiry_is_none_and_extend_restores(self):
        dev, secret = self.make()
        self.clock.now += 7200
        self.assertIsNone(self.store.device_by_secret(secret))
        self.store.extend_device(dev.id, int(self.clock.now) + 100)
        self.assertEqual(self.store.device_by_secret(secret).id, dev.id)

    def test_unknown_secret_is_none(self):
        self.assertIsNone(self.store.device_by_secret("nope"))

    def test_db_file_has_no_plaintext(self):
        dev, secret = self.make()
        tok = self.store.create_token(dev.id)
        with open(self.path, "rb") as f:
            data = f.read()
        for name in (self.path + "-wal",):
            if os.path.exists(name):
                with open(name, "rb") as f:
                    data += f.read()
        self.assertNotIn(secret.encode(), data)
        self.assertNotIn(tok.encode(), data)

    def test_credentials(self):
        live, _ = self.make("live")
        revoked, _ = self.make("rev")
        self.store.revoke_device(revoked.id)
        expired, _ = self.make("exp", expires_at=int(self.clock.now) + 5)
        self.clock.now += 10
        creds = self.store.credentials()
        self.assertEqual(len(creds), 3)
        self.assertEqual({c["mode"] for c in creds}, MODES)
        for c in creds:
            self.assertEqual(set(c), {"uuid", "mode", "not_after"})
            self.assertEqual(c["uuid"], live.uuids[c["mode"]])
            self.assertEqual(c["not_after"], live.expires_at)

    def test_device_has_no_secret_fields(self):
        dev, _ = self.make()
        self.assertEqual(
            set(dev.__dataclass_fields__),
            {"id", "name", "owner", "client", "expires_at", "revoked", "uuids"},
        )

    def test_short_or_empty_key_rejected(self):
        for key in (b"", b"short"):
            with self.assertRaises(ValueError):
                Store(os.path.join(self.tmp.name, "k.db"), key)

    def test_subscription_secret_unknown_device_is_keyerror(self):
        with self.assertRaises(KeyError):
            self.store.subscription_secret(999)

    def test_secret_depends_on_key_and_is_deterministic(self):
        dev, secret = self.make()
        again = Store(self.path, KEY, clock=self.clock)
        self.assertEqual(again.subscription_secret(dev.id), secret)
        other = Store(self.path, b"another-key-0123456789", clock=self.clock)
        self.assertNotEqual(other.subscription_secret(dev.id), secret)
        self.assertIsNone(other.device_by_secret(secret))

    def test_malformed_and_forged_secrets_are_none(self):
        dev, secret = self.make()
        mac = secret.split(".", 1)[1]
        for bad in ("", ".", f"{dev.id}", f"{dev.id}.", f".{mac}", f"x.{mac}", f"{dev.id + 1}.{mac}",
                    f"{dev.id}.{mac[:-1]}A" if not mac.endswith("A") else f"{dev.id}.{mac[:-1]}B",
                    f"{dev.id}.{mac}.extra", "\u00e9.\u00e9", "9" * 25 + ".x", "9" * 5000 + ".x"):
            self.assertIsNone(self.store.device_by_secret(bad), bad)

    def test_rotate_secret(self):
        dev, old = self.make()
        self.store.rotate_secret(dev.id)
        new = self.store.subscription_secret(dev.id)
        self.assertNotEqual(old, new)
        self.assertIsNone(self.store.device_by_secret(old))
        self.assertEqual(self.store.device_by_secret(new).id, dev.id)

    def test_old_db_without_epoch_is_migrated(self):
        path = os.path.join(self.tmp.name, "old.db")
        db = sqlite3.connect(path)
        db.executescript(
            "CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,"
            " owner TEXT NOT NULL, client TEXT NOT NULL, expires_at INTEGER NOT NULL,"
            " revoked INTEGER NOT NULL DEFAULT 0, secret_hash TEXT NOT NULL UNIQUE);"
            "INSERT INTO devices (name, owner, client, expires_at, secret_hash)"
            " VALUES ('a','b','sing-box', 9999999999, 'h');"
        )
        db.commit()
        db.close()
        store = Store(path, KEY, clock=self.clock)
        dev = store.list_devices()[0]
        self.assertEqual(store.device_by_secret(store.subscription_secret(dev.id)).id, dev.id)
        new, secret = store.create_device("n", "o", "sing-box", 9999999999)
        self.assertEqual(store.device_by_secret(secret).id, new.id)

    def test_persists_across_reopen(self):
        dev, secret = self.make()
        again = Store(self.path, KEY, clock=self.clock)
        self.assertEqual(again.device_by_secret(secret), dev)


if __name__ == "__main__":
    unittest.main()

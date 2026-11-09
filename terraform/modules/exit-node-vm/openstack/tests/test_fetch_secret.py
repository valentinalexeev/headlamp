"""Tests for templates/fetch-secret.py: a full fetch from a fake Keystone and Barbican, and the error path.
Run: python3 terraform/modules/exit-node-vm/openstack/tests/test_fetch_secret.py"""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "templates" / "fetch-secret.py"
SECRET = "tskey-auth-k123-SECRETVALUE"


class Fake(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        ident = body["auth"]["identity"]
        ok = ident["methods"] == ["application_credential"] and \
            ident["application_credential"] == {"id": "ac1", "secret": "s3"}
        self.send_response(201 if ok else 401)
        self.send_header("X-Subject-Token", "TOK")
        self.end_headers()

    def do_GET(self):
        ok = self.path == "/key-manager/v1/secrets/abc/payload" and self.headers["X-Auth-Token"] == "TOK"
        self.send_response(200 if ok else 403)
        self.end_headers()
        if ok:
            self.wfile.write((SECRET + "\n").encode())


class FetchSecret(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), Fake)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.srv.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def run_script(self, cred, env=None):
        d = tempfile.mkdtemp()
        c, out = os.path.join(d, "cred.json"), os.path.join(d, "authkey")
        with open(c, "w") as f:
            json.dump(cred, f)
        r = subprocess.run([sys.executable, str(SCRIPT), c, out], capture_output=True, text=True, timeout=120,
                           env={**os.environ, **(env or {})})
        return r, out

    def test_writes_the_payload_with_mode_0600(self):
        r, out = self.run_script({"auth_url": self.url + "/v3", "id": "ac1", "secret": "s3",
                                  "secret_href": self.url + "/key-manager/v1/secrets/abc"})
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(Path(out).read_text(), SECRET)
        self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)

    def test_a_wrong_credential_fails_and_writes_nothing(self):
        r, out = self.run_script({"auth_url": self.url + "/v3", "id": "ac1", "secret": "wrong",
                                  "secret_href": self.url + "/key-manager/v1/secrets/abc"},
                                 env={"HL_FETCH_TRIES": "1"})
        self.assertEqual(r.returncode, 1)
        self.assertIn("401", r.stderr)
        self.assertFalse(os.path.exists(out))


if __name__ == "__main__":
    unittest.main()

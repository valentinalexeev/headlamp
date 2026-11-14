"""Tests for templates/fetch-secret.py: a full fetch from a fake metadata server and Secret Manager.
Run: python3 terraform/modules/exit-node-vm/gcp/tests/test_fetch_secret.py"""
import base64
import json
import os
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "templates" / "fetch-secret.py"
SECRET = "tskey-auth-k123-SECRETVALUE"
NAME = "projects/p1/secrets/n1-authkey"


class Fake(BaseHTTPRequestHandler):
    seen = []

    def log_message(self, *a):
        pass

    def do_GET(self):
        Fake.seen.append((self.path, dict(self.headers)))
        if self.path == "/computeMetadata/v1/instance/service-accounts/default/token":
            if self.headers.get("Metadata-Flavor") != "Google":
                self.send_response(403)
                self.end_headers()
                return
            body = {"access_token": "ya29.fake", "expires_in": 3599, "token_type": "Bearer"}
        elif self.path == "/v1/" + NAME + "/versions/latest:access":
            if self.headers.get("Authorization") != "Bearer ya29.fake":
                self.send_response(401)
                self.end_headers()
                return
            body = {"name": NAME + "/versions/3", "payload": {"data": base64.b64encode((SECRET + "\n").encode()).decode()}}
        else:
            self.send_response(404)
            self.end_headers()
            return
        out = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(out)


class FetchSecretTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), Fake)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.srv.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def run_script(self, name, out):
        env = dict(os.environ, HL_METADATA_URL=self.url, HL_SM_URL=self.url)
        return subprocess.run([sys.executable, str(SCRIPT), name, out], env=env, capture_output=True, text=True,
                              timeout=60)

    def test_writes_the_secret_mode_0600(self):
        with tempfile.TemporaryDirectory() as d:
            out = os.path.join(d, "authkey")
            r = self.run_script(NAME, out)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(Path(out).read_text(), SECRET)
            self.assertEqual(stat.S_IMODE(os.stat(out).st_mode), 0o600)
            self.assertNotIn(SECRET, r.stdout + r.stderr)

    def test_refuses_a_bad_name_without_a_request(self):
        Fake.seen.clear()
        with tempfile.TemporaryDirectory() as d:
            r = self.run_script("n1-authkey", os.path.join(d, "authkey"))
            self.assertNotEqual(r.returncode, 0)
            self.assertEqual(Fake.seen, [])


if __name__ == "__main__":
    unittest.main()

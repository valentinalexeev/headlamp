"""Tests for templates/fetch-secret.py: SigV4 against the AWS test vector, and a full fetch from a fake IMDSv2 and
Secrets Manager.  Run: python3 terraform/modules/exit-node-vm/aws/tests/test_fetch_secret.py"""
import datetime
import importlib.util
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
spec = importlib.util.spec_from_file_location("fetch_secret", SCRIPT)
fs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fs)

SECRET = "tskey-auth-k123-SECRETVALUE"
ARN = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:headlamp/n1-authkey-AbCdEf"


class Fake(BaseHTTPRequestHandler):
    seen = {}

    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype="text/plain"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.end_headers()
        self.wfile.write(body.encode())

    def do_PUT(self):
        if self.path == "/latest/api/token" and self.headers.get("X-aws-ec2-metadata-token-ttl-seconds"):
            return self._send(200, "TOKEN123")
        self._send(400, "bad")

    def do_GET(self):
        if self.headers.get("X-aws-ec2-metadata-token") != "TOKEN123":
            return self._send(401, "IMDSv2 token required")
        routes = {
            "/latest/meta-data/placement/region": "eu-central-1",
            "/latest/meta-data/iam/security-credentials/": "n1-exit-node\n",
            "/latest/meta-data/iam/security-credentials/n1-exit-node": json.dumps(
                {"AccessKeyId": "ASIAEXAMPLE", "SecretAccessKey": "s3cr3t", "Token": "SESSIONTOKEN"}),
        }
        if self.path in routes:
            return self._send(200, routes[self.path])
        self._send(404, "nf")

    def do_POST(self):
        n = int(self.headers["Content-Length"])
        body = json.loads(self.rfile.read(n))
        Fake.seen = {"auth": self.headers.get("Authorization", ""), "target": self.headers.get("X-Amz-Target"),
                     "token": self.headers.get("X-Amz-Security-Token"), "body": body}
        if body.get("SecretId") != ARN:
            return self._send(400, "{}", "application/x-amz-json-1.1")
        self._send(200, json.dumps({"SecretString": SECRET + "\n"}), "application/x-amz-json-1.1")


class Tests(unittest.TestCase):
    def test_sigv4_matches_the_aws_test_vector(self):
        # AWS SigV4 test suite "get-vanilla" (service "service", region us-east-1, 20150830T123600Z).
        now = datetime.datetime(2015, 8, 30, 12, 36, 0, tzinfo=datetime.timezone.utc)
        h = fs.sign("GET", "example.amazonaws.com", "/", "us-east-1", "service", b"", {},
                    "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", None, now)
        self.assertEqual(
            h["authorization"],
            "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, "
            "SignedHeaders=host;x-amz-date, "
            "Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31")

    def test_fetch_writes_the_secret_with_mode_0600(self):
        srv = HTTPServer(("127.0.0.1", 0), Fake)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        url = f"http://127.0.0.1:{srv.server_port}"
        with tempfile.TemporaryDirectory() as d:
            out = os.path.join(d, "authkey")
            env = dict(os.environ, HL_IMDS_URL=url, HL_SM_URL=url + "/")
            r = subprocess.run([sys.executable, str(SCRIPT), ARN, out], env=env, capture_output=True, text=True,
                               timeout=30)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(Path(out).read_text(), SECRET)
            self.assertEqual(stat.S_IMODE(os.stat(out).st_mode), 0o600)
        srv.shutdown()
        self.assertEqual(Fake.seen["target"], "secretsmanager.GetSecretValue")
        self.assertEqual(Fake.seen["token"], "SESSIONTOKEN")
        self.assertIn("Credential=ASIAEXAMPLE/", Fake.seen["auth"])
        self.assertIn("/eu-central-1/secretsmanager/aws4_request", Fake.seen["auth"])
        self.assertIn("x-amz-security-token", Fake.seen["auth"])


if __name__ == "__main__":
    unittest.main(verbosity=2)

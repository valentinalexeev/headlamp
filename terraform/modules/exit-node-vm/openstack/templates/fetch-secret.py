#!/usr/bin/env python3
"""Read one Barbican secret payload with an application credential and write it to a file (mode 0600).

Python standard library only: the image has no OpenStack CLI.
Usage: fetch-secret.py <credential-file> <out-file>
The credential file is JSON: {"auth_url", "id", "secret", "secret_href"}. The application credential has one access
rule: GET on this secret's payload in the key-manager service, so it can read this secret and nothing else.
"""
import json
import os
import sys
import time
import urllib.request


def token(auth_url, cred_id, cred_secret):
    body = json.dumps({"auth": {"identity": {
        "methods": ["application_credential"],
        "application_credential": {"id": cred_id, "secret": cred_secret},
    }}}).encode()
    req = urllib.request.Request(auth_url.rstrip("/") + "/auth/tokens", data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as r:
        return r.headers["X-Subject-Token"]


def payload(secret_href, tok):
    req = urllib.request.Request(secret_href.rstrip("/") + "/payload",
                                 headers={"X-Auth-Token": tok, "Accept": "text/plain"})
    with urllib.request.urlopen(req, timeout=15) as r:
        return r.read().decode().strip()


def write_private(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(value)


def main(argv):
    if len(argv) != 3:
        print("usage: fetch-secret.py <credential-file> <out-file>", file=sys.stderr)
        return 2
    with open(argv[1]) as f:
        c = json.load(f)
    last = None
    # The network can come up after cloud-init starts: try for about 2 minutes. HL_FETCH_TRIES is a test hook.
    tries = int(os.environ.get("HL_FETCH_TRIES", "8"))
    for attempt in range(tries):
        try:
            value = payload(c["secret_href"], token(c["auth_url"], c["id"], c["secret"]))
            if not value:
                raise ValueError("the secret payload is empty")
            write_private(argv[2], value)
            return 0
        except Exception as e:  # noqa: BLE001 - report and try again
            last = e
            if attempt + 1 < tries:
                time.sleep(min(2 ** attempt, 30))
    print(f"fetch-secret: {last}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))

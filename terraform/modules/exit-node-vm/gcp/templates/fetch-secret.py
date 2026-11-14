#!/usr/bin/env python3
"""Read the latest version of one Secret Manager secret with the VM's service account, and write it to a file (0600).

Python standard library only: Ubuntu Minimal has no gcloud. The access token comes from the metadata server.
Usage: fetch-secret.py projects/<project>/secrets/<name> <out-file>
Test hooks: HL_METADATA_URL and HL_SM_URL override the metadata server and the Secret Manager endpoint.
"""
import base64
import json
import os
import re
import sys
import time
import urllib.request

METADATA = os.environ.get("HL_METADATA_URL", "http://metadata.google.internal")
SM = os.environ.get("HL_SM_URL", "https://secretmanager.googleapis.com")
NAME = re.compile(r"^projects/[^/]+/secrets/[^/]+$")


def token():
    req = urllib.request.Request(METADATA + "/computeMetadata/v1/instance/service-accounts/default/token",
                                 headers={"Metadata-Flavor": "Google"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read())["access_token"]


def get_secret(name):
    req = urllib.request.Request(SM + "/v1/" + name + "/versions/latest:access",
                                 headers={"Authorization": "Bearer " + token()})
    with urllib.request.urlopen(req, timeout=10) as r:
        return base64.b64decode(json.loads(r.read())["payload"]["data"]).decode().strip()


def main():
    name, out = sys.argv[1], sys.argv[2]
    if not NAME.match(name):
        sys.exit("fetch-secret: the secret must be projects/<project>/secrets/<name>")
    last = None
    for attempt in range(10):  # the service account binding can take some seconds to reach a new VM
        try:
            value = get_secret(name)
            break
        except Exception as e:  # noqa: BLE001 - retry any network or metadata error
            last = e
            time.sleep(min(2 ** attempt, 30))
    else:
        # The type only: an HTTP error body could echo request data.
        sys.exit(f"fetch-secret: cannot read the secret: {type(last).__name__}")
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(value)


if __name__ == "__main__":
    main()

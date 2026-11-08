#!/usr/bin/env python3
"""Read one Secrets Manager secret with the instance role and write its SecretString to a file (mode 0600).

Python standard library only: Ubuntu Minimal has no AWS CLI. Credentials and the region come from IMDSv2.
Usage: fetch-secret.py <secret-arn> <out-file>
Test hooks: HL_IMDS_URL and HL_SM_URL override the metadata and the Secrets Manager endpoints.
"""
import datetime
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.request

IMDS = os.environ.get("HL_IMDS_URL", "http://169.254.169.254")


def imds(path, token):
    req = urllib.request.Request(IMDS + path, headers={"X-aws-ec2-metadata-token": token})
    with urllib.request.urlopen(req, timeout=5) as r:
        return r.read().decode()


def imds_token():
    req = urllib.request.Request(IMDS + "/latest/api/token", method="PUT",
                                 headers={"X-aws-ec2-metadata-token-ttl-seconds": "300"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return r.read().decode()


def credentials():
    tok = imds_token()
    region = imds("/latest/meta-data/placement/region", tok)
    role = imds("/latest/meta-data/iam/security-credentials/", tok).splitlines()[0].strip()
    c = json.loads(imds("/latest/meta-data/iam/security-credentials/" + role, tok))
    return region, c["AccessKeyId"], c["SecretAccessKey"], c.get("Token")


def _hmac(key, msg):
    return hmac.new(key, msg.encode(), hashlib.sha256).digest()


def sign(method, host, path, region, service, body, headers, key_id, secret, token, now):
    """AWS Signature Version 4. Returns the headers to send (with Authorization)."""
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    day = now.strftime("%Y%m%d")
    h = {k.lower(): v for k, v in headers.items()}
    h["host"] = host
    h["x-amz-date"] = amz_date
    if token:
        h["x-amz-security-token"] = token
    names = sorted(h)
    canonical_headers = "".join(f"{n}:{h[n].strip()}\n" for n in names)
    signed = ";".join(names)
    payload_hash = hashlib.sha256(body).hexdigest()
    creq = "\n".join([method, path, "", canonical_headers, signed, payload_hash])
    scope = f"{day}/{region}/{service}/aws4_request"
    sts = "\n".join(["AWS4-HMAC-SHA256", amz_date, scope, hashlib.sha256(creq.encode()).hexdigest()])
    k = _hmac(_hmac(_hmac(_hmac(("AWS4" + secret).encode(), day), region), service), "aws4_request")
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    h["authorization"] = f"AWS4-HMAC-SHA256 Credential={key_id}/{scope}, SignedHeaders={signed}, Signature={sig}"
    return h


def get_secret(arn):
    region, key_id, secret, token = credentials()
    host = f"secretsmanager.{region}.amazonaws.com"
    url = os.environ.get("HL_SM_URL", f"https://{host}/")
    body = json.dumps({"SecretId": arn}).encode()
    hdrs = sign("POST", host, "/", region, "secretsmanager", body,
                {"content-type": "application/x-amz-json-1.1", "x-amz-target": "secretsmanager.GetSecretValue"},
                key_id, secret, token, datetime.datetime.now(datetime.timezone.utc))
    hdrs.pop("host")
    req = urllib.request.Request(url, data=body, method="POST", headers=hdrs)
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.loads(r.read())["SecretString"].strip()


def main():
    arn, out = sys.argv[1], sys.argv[2]
    last = None
    for attempt in range(10):  # the role can take some seconds to reach a new instance
        try:
            value = get_secret(arn)
            break
        except Exception as e:  # noqa: BLE001 - retry any network or IMDS error
            last = e
            time.sleep(min(2 ** attempt, 30))
    else:
        sys.exit(f"fetch-secret: cannot read the secret: {type(last).__name__}: {last}")
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(value)


if __name__ == "__main__":
    main()

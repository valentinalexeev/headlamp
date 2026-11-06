"""Regenerates testdata/oci_signature_vectors.json with the official OCI SDK signer.

Not run by CI. Needs `pip install oci`. A throwaway RSA key is generated in memory
and never written anywhere; only the PUBLIC key, the exact signing strings and the
SDK's signatures are stored. RSA PKCS#1 v1.5 is deterministic, so the Go test can
check that sha256(<signing string Go builds>) verifies against the SDK's signature
with the public key: that proves the signing string and the scheme match the SDK
byte for byte without any private key in the repository.

    python3 scripts/gen_oci_vectors.py
"""
import base64
import json
import pathlib
import re

import oci
import requests
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa

DATE = "Sat, 24 Oct 2026 12:00:00 GMT"
TENANCY = "ocid1.tenancy.oc1..exampletenancy"
USER = "ocid1.user.oc1..exampleuser"
FINGERPRINT = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
BASE = "https://iaas.region-1.oraclecloud.com"

key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
# The SDK wants PEM text; it is produced by the library and lives only in memory.
pem = key.private_bytes(
    serialization.Encoding.PEM, serialization.PrivateFormat.TraditionalOpenSSL, serialization.NoEncryption()
).decode()
pub = key.public_key()
pub_der = pub.public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)

signer = oci.signer.Signer(TENANCY, USER, FINGERPRINT, None, private_key_content=pem)

cases = [
    ("get_list", "GET", "/20160918/instances?compartmentId=ocid1.compartment.oc1..examplecomp&displayName=web-1", None),
    ("post_body", "POST", "/20160918/instances/ocid1.instance.oc1..exampleinst/actions", '{"a": 1, "b": "x"}'),
    ("post_empty", "POST", "/20160918/instances/ocid1.instance.oc1..exampleinst?action=STOP", None),
]


def signing_string(method, path, headers, names):
    lines = []
    for n in names:
        if n == "(request-target)":
            lines.append(f"(request-target): {method.lower()} {path}")
        else:
            lines.append(f"{n}: {headers[n]}")
    return "\n".join(lines)


out = {
    "_comment": "Public key, signing strings and signatures from the official OCI SDK signer; no private key is stored.",
    "key_id": f"{TENANCY}/{USER}/{FINGERPRINT}",
    "public_key_pkix_der_b64": base64.b64encode(pub_der).decode(),
    "date": DATE,
    "cases": [],
}
for name, method, path, body in cases:
    req = requests.Request(method, BASE + path, data=body, headers={"date": DATE}).prepare()
    signer(req)
    auth = req.headers["authorization"]
    names = re.search(r'headers="([^"]*)"', auth).group(1).split(" ")
    sig = base64.b64decode(re.search(r'signature="([^"]*)"', auth).group(1))
    hdrs = {k.lower(): v for k, v in req.headers.items()}
    hdrs["host"] = BASE.split("//", 1)[1]
    sstr = signing_string(method, path, hdrs, names)
    # Self-check: our reconstruction of the signing string is what the SDK signed.
    pub.verify(sig, sstr.encode(), padding.PKCS1v15(), hashes.SHA256())
    keep = ["date", "host", "authorization", "x-content-sha256", "content-type", "content-length"]
    out["cases"].append({
        "name": name, "method": method, "url": BASE + path, "body": body or "",
        "headers": {k: v for k, v in ((h, hdrs.get(h)) for h in keep) if v is not None},
        "signing_string": sstr,
        "signature_b64": base64.b64encode(sig).decode(),
    })
pathlib.Path(__file__).resolve().parent.parent.joinpath("testdata", "oci_signature_vectors.json").write_text(
    json.dumps(out, indent=2) + "\n")

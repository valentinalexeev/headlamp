#!/usr/bin/env python3
"""Copy files out of a container image that is pinned by digest (#182).

Usage: oci-extract.py [--arch amd64|arm64] <registry>/<repository>@sha256:<digest> <dest dir> <path in image> [...]

Standard library only, so it runs with the Python of ansible-playbook: no docker, skopeo or crane.
The registry is accessed anonymously (the Bearer token flow of the OCI distribution spec). The digest
may name an image index (the linux/<arch> manifest is used, default amd64) or a manifest. Every manifest and every layer is checked against
its digest before use. A file is taken from the newest layer that has it. Each file goes to
<dest dir>/<basename>. The caller still checks the sha256 of each file it installs.
"""
import hashlib
import json
import os
import re
import shutil
import ssl
import sys
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request

TIMEOUT_SECONDS = 120
MANIFEST_SIZE_MAX_BYTES = 4 << 20
LAYER_SIZE_MAX_BYTES = 1 << 30
LAYER_COUNT_MAX = 64
REDIRECT_COUNT_MAX = 5
CHUNK_BYTES = 1 << 20
ARCHITECTURES = ("amd64", "arm64")

MANIFEST_TYPES = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])
INDEX_TYPES = ("application/vnd.oci.image.index.v1+json",
               "application/vnd.docker.distribution.manifest.list.v2+json")


def fail(message):
    sys.stderr.write("oci-extract: %s\n" % message)
    sys.exit(1)


def parse_reference(reference):
    match = re.fullmatch(r"([a-z0-9.-]+(?::[0-9]+)?)/([a-z0-9._/-]+)@(sha256:[0-9a-f]{64})", reference)
    if not match:
        fail("image must be <registry>/<repository>@sha256:<64 hex>, got %r" % reference)
    registry, repository, digest = match.groups()
    if registry == "docker.io":
        registry = "registry-1.docker.io"
    return registry, repository, digest


class NoAuthOnRedirect(urllib.request.HTTPRedirectHandler):
    """Blob downloads redirect to a CDN, which rejects the registry token."""
    max_redirections = REDIRECT_COUNT_MAX

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None:
            new.remove_header("Authorization")
        return new


def tls_context():
    """Some Python builds (python.org on macOS) have no CA store; certifi fills it when installed."""
    try:
        import certifi  # pylint: disable=import-outside-toplevel
        return ssl.create_default_context(cafile=certifi.where())
    except ImportError:
        return ssl.create_default_context()


OPENER = urllib.request.build_opener(NoAuthOnRedirect, urllib.request.HTTPSHandler(context=tls_context()))


class Registry:
    def __init__(self, registry, repository):
        self.base = "https://%s/v2/%s" % (registry, repository)
        self.token = None

    def open(self, path, accept):
        request = urllib.request.Request(self.base + path, headers={"Accept": accept})
        if self.token:
            request.add_header("Authorization", "Bearer " + self.token)
        try:
            return OPENER.open(request, timeout=TIMEOUT_SECONDS)
        except urllib.error.HTTPError as error:
            if error.code != 401 or self.token:
                fail("GET %s: HTTP %d" % (path, error.code))
            self.login(error.headers.get("WWW-Authenticate", ""))
            return self.open(path, accept)

    def login(self, challenge):
        if not challenge.startswith("Bearer "):
            fail("registry wants unsupported auth: %r" % challenge)
        params = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
        realm = params.pop("realm", "")
        if not realm.startswith("https://"):
            fail("registry token realm is not https: %r" % realm)
        url = realm + "?" + urllib.parse.urlencode(params)
        with OPENER.open(url, timeout=TIMEOUT_SECONDS) as response:
            body = json.loads(response.read(MANIFEST_SIZE_MAX_BYTES))
        self.token = body.get("token") or body.get("access_token")
        if not self.token:
            fail("registry gave no token")

    def manifest(self, digest):
        with self.open("/manifests/" + digest, MANIFEST_TYPES) as response:
            body = response.read(MANIFEST_SIZE_MAX_BYTES + 1)
        if len(body) > MANIFEST_SIZE_MAX_BYTES:
            fail("manifest %s is too large" % digest)
        if "sha256:" + hashlib.sha256(body).hexdigest() != digest:
            fail("manifest digest mismatch for %s" % digest)
        return json.loads(body)

    def blob(self, descriptor, dest):
        digest, size = descriptor["digest"], descriptor.get("size", 0)
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest) or size > LAYER_SIZE_MAX_BYTES:
            fail("unsupported or too large layer %s (%d bytes)" % (digest, size))
        hasher, total = hashlib.sha256(), 0
        with self.open("/blobs/" + digest, "*/*") as response, open(dest, "wb") as out:
            while True:
                chunk = response.read(CHUNK_BYTES)
                if not chunk:
                    break
                total += len(chunk)
                if total > LAYER_SIZE_MAX_BYTES:
                    fail("layer %s is larger than %d bytes" % (digest, LAYER_SIZE_MAX_BYTES))
                hasher.update(chunk)
                out.write(chunk)
        if "sha256:" + hasher.hexdigest() != digest:
            fail("layer digest mismatch for %s" % digest)


def platform_manifest(registry, digest, architecture):
    manifest = registry.manifest(digest)
    if manifest.get("mediaType") in INDEX_TYPES or "manifests" in manifest:
        for entry in manifest.get("manifests", []):
            platform = entry.get("platform", {})
            if (platform.get("os"), platform.get("architecture")) == ("linux", architecture):
                return registry.manifest(entry["digest"])
        fail("image index %s has no linux/%s manifest" % (digest, architecture))
    return manifest


def extract(registry, layers, wanted, dest_dir, work_dir):
    """Newest layer first: the first layer that has a path wins."""
    remaining = {path.lstrip("/"): path for path in wanted}
    for index, layer in reversed(list(enumerate(layers))):
        if not remaining:
            break
        blob_path = os.path.join(work_dir, "layer-%d" % index)
        registry.blob(layer, blob_path)
        with tarfile.open(blob_path, "r:*") as archive:
            for member in archive:
                name = member.name.lstrip("./").lstrip("/")
                if name not in remaining or not member.isfile():
                    continue
                source = archive.extractfile(member)
                target = os.path.join(dest_dir, os.path.basename(name))
                with open(target, "wb") as out:
                    shutil.copyfileobj(source, out, CHUNK_BYTES)
                os.chmod(target, 0o600)
                del remaining[name]
        os.remove(blob_path)
    if remaining:
        fail("not found in the image: %s" % ", ".join(sorted(remaining.values())))


def main(argv):
    architecture = "amd64"
    if len(argv) > 2 and argv[1] == "--arch":
        architecture = argv[2]
        argv = argv[:1] + argv[3:]
    if architecture not in ARCHITECTURES:
        fail("--arch must be one of %s, got %r" % (", ".join(ARCHITECTURES), architecture))
    if len(argv) < 4:
        fail("usage: oci-extract.py [--arch amd64|arm64] <image@sha256:...> <dest dir> <path> [...]")
    registry_name, repository, digest = parse_reference(argv[1])
    dest_dir = argv[2]
    if not os.path.isdir(dest_dir):
        fail("destination %s is not a directory" % dest_dir)
    registry = Registry(registry_name, repository)
    layers = platform_manifest(registry, digest, architecture).get("layers", [])
    if not layers or len(layers) > LAYER_COUNT_MAX:
        fail("image has %d layers (1 to %d allowed)" % (len(layers), LAYER_COUNT_MAX))
    with tempfile.TemporaryDirectory(prefix="oci-extract.", dir=dest_dir) as work_dir:
        extract(registry, layers, argv[3:], dest_dir, work_dir)


if __name__ == "__main__":
    main(sys.argv)

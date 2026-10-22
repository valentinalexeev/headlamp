"""Pure credential-set helpers and persistence. Holds only uuid, mode and expiry."""
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Cred:
    uuid: str
    mode: str
    not_after: int


def diff(current: list[Cred], desired: list[Cred]) -> tuple[list[Cred], list[str]]:
    """Return (creds to add, uuids to remove). A changed mode counts as remove + add."""
    cur, want = set(current), set(desired)
    added = [c for c in desired if c not in cur]
    removed = sorted({c.uuid for c in current if c not in want})
    return added, removed


def sweep(creds: list[Cred], now: int) -> list[Cred]:
    return [c for c in creds if c.not_after > now]


def load(path: Path) -> list[Cred]:
    try:
        raw = json.loads(Path(path).read_text())
    except FileNotFoundError:
        return []
    return [Cred(r["uuid"], r["mode"], int(r["not_after"])) for r in raw]


def write_atomic(path: Path, text: str, mode: int = 0o600) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def save(path: Path, creds: list[Cred]) -> None:
    rows = [{"uuid": c.uuid, "mode": c.mode, "not_after": c.not_after} for c in creds]
    write_atomic(path, json.dumps(rows), 0o600)

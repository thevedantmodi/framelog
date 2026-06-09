import hashlib
from pathlib import Path


def hash_file(path: Path) -> str:
    """Return the SHA-256 hex digest of the file at path, read in 8 KB chunks."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8192), b""):
            h.update(chunk)
    return h.hexdigest()

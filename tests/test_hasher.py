import hashlib
from pathlib import Path

import pytest

from framelog.hasher import hash_file


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def test_known_content(tmp_path: Path):
    data = b"fuji xt5 raw photo"
    f = tmp_path / "photo.raf"
    f.write_bytes(data)
    assert hash_file(f) == _sha256(data)


def test_idempotent(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"same content")
    assert hash_file(f) == hash_file(f)


def test_different_content(tmp_path: Path):
    a = tmp_path / "a.raf"
    b = tmp_path / "b.raf"
    a.write_bytes(b"photo a")
    b.write_bytes(b"photo b")
    assert hash_file(a) != hash_file(b)


def test_empty_file(tmp_path: Path):
    f = tmp_path / "empty.raf"
    f.write_bytes(b"")
    assert hash_file(f) == _sha256(b"")

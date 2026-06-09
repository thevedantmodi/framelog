import sqlite3
from pathlib import Path

import pytest

from framelog.db import hash_exists, init_db, insert_photo, update_status

_RECORD = {
    "hash": "abc123",
    "original_filename": "DSF0001.RAF",
    "imported_path": "/Photos/originals/2026/05/28/20260528_120000_abc123.raf",
    "camera_model": "X-T5",
    "capture_date": "2026:05:28 12:00:00",
    "import_timestamp": "2026-05-28T12:00:00",
}


@pytest.fixture
def db(tmp_path: Path) -> Path:
    path = tmp_path / "catalog.db"
    init_db(db_path=path)
    return path


def test_init_db_creates_table(db: Path):
    with sqlite3.connect(db) as conn:
        rows = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name='photos'"
        ).fetchall()
    assert len(rows) == 1


def test_init_db_idempotent(tmp_path: Path):
    path = tmp_path / "catalog.db"
    init_db(db_path=path)
    init_db(db_path=path)  # should not raise


def test_hash_exists_false_when_empty(db: Path):
    assert hash_exists("nonexistent", db_path=db) is False


def test_hash_exists_true_after_insert(db: Path):
    insert_photo(_RECORD, db_path=db)
    assert hash_exists("abc123", db_path=db) is True


def test_insert_photo_round_trip(db: Path):
    insert_photo(_RECORD, db_path=db)
    with sqlite3.connect(db) as conn:
        conn.row_factory = sqlite3.Row
        row = conn.execute("SELECT * FROM photos WHERE hash = 'abc123'").fetchone()
    assert row["original_filename"] == "DSF0001.RAF"
    assert row["status"] == "raw"


def test_update_status(db: Path):
    insert_photo(_RECORD, db_path=db)
    update_status("abc123", "edited", db_path=db)
    with sqlite3.connect(db) as conn:
        row = conn.execute("SELECT status FROM photos WHERE hash = 'abc123'").fetchone()
    assert row[0] == "edited"


def test_duplicate_insert_raises(db: Path):
    insert_photo(_RECORD, db_path=db)
    with pytest.raises(sqlite3.IntegrityError):
        insert_photo(_RECORD, db_path=db)

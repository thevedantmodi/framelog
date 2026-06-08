from datetime import datetime
from pathlib import Path
from unittest.mock import patch

import pytest

from db import init_db
from ingest import import_file, run_ingest

_EXIF = {
    "capture_date": "2026:05:28 12:00:00",
    "camera_model": "X-T5",
    "gps_lat": None,
    "gps_lon": None,
}

_EXIF_IPHONE = {
    "capture_date": "2026:05:28 12:00:00",
    "camera_model": "iPhone 15 Pro",
    "gps_lat": None,
    "gps_lon": None,
}


@pytest.fixture
def dirs(tmp_path: Path):
    inbox = tmp_path / "inbox"
    originals = tmp_path / "originals"
    db = tmp_path / "catalog.db"
    inbox.mkdir()
    originals.mkdir()
    init_db(db_path=db)
    return {"inbox": inbox, "originals": originals, "db": db}


def _make_photo(inbox: Path, name: str = "DSF0001.RAF") -> Path:
    p = inbox / name
    p.write_bytes(b"fake raw data")
    return p


def test_import_copies_to_dest(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    photo = _make_photo(dirs["inbox"])
    with patch("ingest.read_exif", return_value=_EXIF):
        result = import_file(photo, "batch-1", db_path=dirs["db"])
    assert result == "imported"
    matches = list(dirs["originals"].rglob("*.raf"))
    assert len(matches) == 1
    dest = matches[0]
    assert dest.parent.parent.parent.name == "2026"  # YYYY
    assert dest.stem.startswith("20260528_120000_")


def test_import_deletes_source(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    photo = _make_photo(dirs["inbox"])
    with patch("ingest.read_exif", return_value=_EXIF):
        import_file(photo, "batch-1", db_path=dirs["db"])
    assert not photo.exists()


def test_import_skips_duplicate(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    photo = _make_photo(dirs["inbox"])
    with patch("ingest.read_exif", return_value=_EXIF):
        import_file(photo, "batch-1", db_path=dirs["db"])
    # re-create source with same content (same hash)
    photo2 = _make_photo(dirs["inbox"], "DSF0002.RAF")
    photo2.write_bytes(b"fake raw data")
    with patch("ingest.read_exif", return_value=_EXIF):
        result = import_file(photo2, "batch-1", db_path=dirs["db"])
    assert result == "skipped"


def test_import_fails_gracefully(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    photo = _make_photo(dirs["inbox"])
    with patch("ingest.read_exif", side_effect=RuntimeError("exiftool failed")):
        result = import_file(photo, "batch-1", db_path=dirs["db"])
    assert result == "failed"
    assert photo.exists()  # source untouched


def test_iphone_source_device(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    photo = _make_photo(dirs["inbox"], "IMG_0001.HEIC")
    photo.write_bytes(b"fake heic data")
    with patch("ingest.read_exif", return_value=_EXIF_IPHONE):
        import_file(photo, "batch-1", db_path=dirs["db"])
    import sqlite3
    with sqlite3.connect(dirs["db"]) as conn:
        row = conn.execute("SELECT source_device FROM photos").fetchone()
    assert row[0] == "iPhone"


def test_run_ingest_skips_unsupported(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    monkeypatch.setattr("ingest.INBOX", dirs["inbox"])
    (dirs["inbox"] / "notes.txt").write_text("not a photo")
    _make_photo(dirs["inbox"])
    with patch("ingest.read_exif", return_value=_EXIF), patch("ingest.git_commit", return_value=False):
        counts = run_ingest(inbox=dirs["inbox"], db_path=dirs["db"])
    assert counts["imported"] == 1
    assert counts["failed"] == 0


def test_run_ingest_counts(dirs, monkeypatch):
    monkeypatch.setattr("ingest.ORIGINALS", dirs["originals"])
    for i in range(3):
        _make_photo(dirs["inbox"], f"DSF{i:04d}.RAF").write_bytes(f"unique {i}".encode())
    with patch("ingest.read_exif", return_value=_EXIF), patch("ingest.git_commit", return_value=False):
        counts = run_ingest(inbox=dirs["inbox"], db_path=dirs["db"])
    assert counts["imported"] == 3
    assert counts["skipped"] == 0

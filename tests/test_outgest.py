from pathlib import Path
from unittest.mock import patch

import pytest

from framelog.outgest import organize_file, run_outgest

_EXIF = {
    "capture_date": "2026:06:07 15:30:00",
    "camera_model": "X-T5",
    "gps_lat": None,
    "gps_lon": None,
}


@pytest.fixture
def processed(tmp_path: Path) -> Path:
    d = tmp_path / "processed"
    d.mkdir()
    return d


def _flat_export(processed: Path, name: str = "export_001.jpg") -> Path:
    p = processed / name
    p.write_bytes(b"fake jpg")
    return p


def test_moves_to_yyyy_mm(processed):
    photo = _flat_export(processed)
    with patch("framelog.outgest.read_exif", return_value=_EXIF):
        result = organize_file(photo)
    assert result == "moved"
    assert not photo.exists()
    dest = processed / "2026" / "06" / "export_001.jpg"
    assert dest.exists()


def test_skips_existing(processed):
    photo = _flat_export(processed)
    dest_dir = processed / "2026" / "06"
    dest_dir.mkdir(parents=True)
    (dest_dir / "export_001.jpg").write_bytes(b"already there")
    with patch("framelog.outgest.read_exif", return_value=_EXIF):
        result = organize_file(photo)
    assert result == "skipped"
    assert photo.exists()


def test_fails_gracefully(processed):
    photo = _flat_export(processed)
    with patch("framelog.outgest.read_exif", side_effect=RuntimeError("exiftool failed")):
        result = organize_file(photo)
    assert result == "failed"
    assert photo.exists()


def test_run_outgest_skips_subdirs(processed):
    subdir = processed / "2026" / "05"
    subdir.mkdir(parents=True)
    (subdir / "nested.jpg").write_bytes(b"nested")
    flat = _flat_export(processed)
    with patch("framelog.outgest.read_exif", return_value=_EXIF):
        counts = run_outgest(processed)
    assert counts["moved"] == 1
    assert counts["failed"] == 0


def test_run_outgest_counts(processed):
    for i in range(3):
        _flat_export(processed, f"export_{i:03d}.jpg").write_bytes(f"img {i}".encode())
    with patch("framelog.outgest.read_exif", return_value=_EXIF):
        counts = run_outgest(processed)
    assert counts["moved"] == 3
    assert counts["skipped"] == 0

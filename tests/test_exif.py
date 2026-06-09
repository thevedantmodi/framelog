import json
import subprocess
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from framelog.exif import read_exif


def _mock_result(data: dict, returncode: int = 0, stderr: str = "") -> MagicMock:
    m = MagicMock()
    m.returncode = returncode
    m.stdout = json.dumps([data])
    m.stderr = stderr
    return m


def test_full_metadata(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"fake")
    exif_data = {
        "DateTimeOriginal": "2026:05:28 12:00:00",
        "Model": "X-T5",
        "GPSLatitude": 37.7749,
        "GPSLongitude": -122.4194,
    }
    with patch("subprocess.run", return_value=_mock_result(exif_data)):
        result = read_exif(f)
    assert result["capture_date"] == "2026:05:28 12:00:00"
    assert result["camera_model"] == "X-T5"
    assert result["gps_lat"] == 37.7749
    assert result["gps_lon"] == -122.4194


def test_missing_date_falls_back_to_mtime(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"fake")
    with patch("subprocess.run", return_value=_mock_result({"Model": "X-T5"})):
        result = read_exif(f)
    assert result["capture_date"] is not None
    assert ":" in result["capture_date"]  # formatted as %Y:%m:%d %H:%M:%S


def test_gps_absent_returns_none(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"fake")
    with patch("subprocess.run", return_value=_mock_result({"DateTimeOriginal": "2026:05:28 12:00:00"})):
        result = read_exif(f)
    assert result["gps_lat"] is None
    assert result["gps_lon"] is None


def test_exiftool_failure_raises(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"fake")
    with patch("subprocess.run", return_value=_mock_result({}, returncode=1, stderr="Error: file not found")):
        with pytest.raises(RuntimeError, match="exiftool failed"):
            read_exif(f)

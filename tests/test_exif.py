import json
import subprocess
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from framelog.exif import _parse_gps, read_exif


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


def test_gps_dms_string(tmp_path: Path):
    f = tmp_path / "photo.heic"
    f.write_bytes(b"fake")
    exif_data = {
        "DateTimeOriginal": "2026:06:04 14:57:09",
        "Model": "iPhone 16 Pro",
        "GPSLatitude": "36 deg 37' 3.55\" N",
        "GPSLongitude": "121 deg 54' 22.10\" W",
    }
    with patch("subprocess.run", return_value=_mock_result(exif_data)):
        result = read_exif(f)
    assert result["gps_lat"] == pytest.approx(36.617652, rel=1e-4)
    assert result["gps_lon"] == pytest.approx(-121.906139, rel=1e-4)


def test_parse_gps_decimal():
    assert _parse_gps(37.7749) == pytest.approx(37.7749)


def test_parse_gps_dms_north():
    assert _parse_gps("36 deg 37' 3.55\" N") == pytest.approx(36.617652, rel=1e-4)


def test_parse_gps_dms_west():
    assert _parse_gps("121 deg 54' 22.10\" W") == pytest.approx(-121.906139, rel=1e-4)


def test_parse_gps_malformed():
    assert _parse_gps("not a coordinate") is None


def test_exiftool_failure_raises(tmp_path: Path):
    f = tmp_path / "photo.raf"
    f.write_bytes(b"fake")
    with patch("subprocess.run", return_value=_mock_result({}, returncode=1, stderr="Error: file not found")):
        with pytest.raises(RuntimeError, match="exiftool failed"):
            read_exif(f)

import json
import subprocess
from datetime import datetime
from pathlib import Path


def read_exif(path: Path) -> dict:
    """Extract metadata from a photo via exiftool. Raises RuntimeError if exiftool fails.

    Returns keys: capture_date (str), camera_model (str|None), gps_lat (float|None), gps_lon (float|None).
    Falls back to file mtime if DateTimeOriginal is absent.
    """
    result = subprocess.run(
        ["/opt/homebrew/bin/exiftool", "-json", str(path)],
        capture_output=True, text=True, check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(f"exiftool failed: {result.stderr.strip()}")
    data = json.loads(result.stdout)[0]
    capture_date = data.get("DateTimeOriginal") or _mtime_str(path)
    return {
        "capture_date": capture_date,
        "camera_model": data.get("Model"),
        "gps_lat": data.get("GPSLatitude"),
        "gps_lon": data.get("GPSLongitude"),
    }


def _mtime_str(path: Path) -> str:
    return datetime.fromtimestamp(path.stat().st_mtime).strftime("%Y:%m:%d %H:%M:%S")

import json
import re
import shutil
import subprocess
from datetime import datetime
from pathlib import Path

_EXIFTOOL_CANDIDATES = [
    "/opt/homebrew/bin/exiftool",   # Homebrew Apple Silicon
    "/usr/local/bin/exiftool",      # Homebrew Intel
    "/usr/bin/exiftool",            # system
]


def _find_exiftool() -> str:
    for candidate in _EXIFTOOL_CANDIDATES:
        if Path(candidate).exists():
            return candidate
    found = shutil.which("exiftool")
    if found:
        return found
    raise RuntimeError(
        "exiftool not found. Install it with: brew install exiftool"
    )


def read_exif(path: Path) -> dict[str, str | float | None]:
    """Extract metadata from a photo via exiftool. Raises RuntimeError if exiftool fails.

    Returns keys: capture_date (str), camera_model (str|None), gps_lat (float|None), gps_lon (float|None).
    Falls back to file mtime if DateTimeOriginal is absent.
    """
    result = subprocess.run(
        [_find_exiftool(), "-json", str(path)],
        capture_output=True, text=True, check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(f"exiftool failed: {result.stderr.strip()}")
    data: dict[str, object] = json.loads(result.stdout)[0]
    capture_date = str(data.get("DateTimeOriginal") or _mtime_str(path))
    return {
        "capture_date": capture_date,
        "camera_model": str(data["Model"]) if "Model" in data else None,
        "gps_lat": _parse_gps(data["GPSLatitude"]) if "GPSLatitude" in data else None,
        "gps_lon": _parse_gps(data["GPSLongitude"]) if "GPSLongitude" in data else None,
    }


def _mtime_str(path: Path) -> str:
    return datetime.fromtimestamp(path.stat().st_mtime).strftime("%Y:%m:%d %H:%M:%S")


def _parse_gps(value: object) -> float | None:
    """Parse exiftool GPS value to decimal degrees. Handles both float and DMS strings."""
    try:
        return float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        pass
    # DMS format: "36 deg 37' 3.55\" N" or "120 deg 5' 10.00\" W"
    m = re.match(r"(\d+)\s+deg\s+(\d+)'\s+([\d.]+)\"\s*([NSEW])", str(value))
    if not m:
        return None
    deg, minutes, secs, direction = m.groups()
    decimal = float(deg) + float(minutes) / 60 + float(secs) / 3600
    if direction in ("S", "W"):
        decimal = -decimal
    return decimal

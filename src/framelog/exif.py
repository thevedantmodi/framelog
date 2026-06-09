import json
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
        "gps_lat": float(data["GPSLatitude"]) if "GPSLatitude" in data else None,  # pyright: ignore[reportArgumentType]
        "gps_lon": float(data["GPSLongitude"]) if "GPSLongitude" in data else None,  # pyright: ignore[reportArgumentType]
    }


def _mtime_str(path: Path) -> str:
    return datetime.fromtimestamp(path.stat().st_mtime).strftime("%Y:%m:%d %H:%M:%S")

import shutil
from datetime import datetime
from pathlib import Path

from config import PROCESSED, SUPPORTED_EXTENSIONS
from exif import read_exif


def organize_file(path: Path) -> str:
    """Move a flat export into processed/YYYY/MM/. Returns 'moved' or 'failed'."""
    try:
        exif = read_exif(path)
        dt = datetime.strptime(str(exif["capture_date"]), "%Y:%m:%d %H:%M:%S")
        dest_dir = path.parent / f"{dt.year:04d}" / f"{dt.month:02d}"
        dest = dest_dir / path.name
        if dest.exists():
            return "skipped"
        dest_dir.mkdir(parents=True, exist_ok=True)
        shutil.move(str(path), dest)
        return "moved"
    except Exception as exc:
        print(f"  FAILED {path.name}: {exc}")
        return "failed"


def run_outgest(processed: Path = PROCESSED) -> dict[str, int]:
    """Move flat exports in processed/ into YYYY/MM/ subdirectories. Returns counts dict."""
    moved = skipped = failed = 0
    for path in sorted(processed.iterdir()):
        if not path.is_file():
            continue
        if path.suffix.lower() not in SUPPORTED_EXTENSIONS:
            continue
        result = organize_file(path)
        if result == "moved":
            moved += 1
        elif result == "skipped":
            skipped += 1
        else:
            failed += 1

    print(f"Done: {moved} moved, {skipped} skipped, {failed} failed")
    return {"moved": moved, "skipped": skipped, "failed": failed}


if __name__ == "__main__":
    run_outgest()

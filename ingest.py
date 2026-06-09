import shutil
from datetime import datetime
from pathlib import Path

from config import DB_PATH, INBOX, ORIGINALS, SUPPORTED_EXTENSIONS, log
from db import hash_exists, init_db, insert_photo
from exif import read_exif
from git import git_commit, git_push
from hasher import hash_file
from xmp import write_xmp


def import_file(path: Path, batch_id: str, db_path: Path = DB_PATH) -> str:
    """Copy a photo from inbox to originals, write XMP sidecar, and record in catalog.

    Returns 'imported', 'skipped' (duplicate hash), or 'failed' (source left untouched).
    """
    file_hash = hash_file(path)

    if hash_exists(file_hash, db_path=db_path):
        return "skipped"

    try:
        exif = read_exif(path)
        camera_model = (
            str(exif["camera_model"]) if exif["camera_model"] is not None else None
        )

        capture_date = str(exif["capture_date"])
        dt = datetime.strptime(capture_date, "%Y:%m:%d %H:%M:%S")
        dest = (
            ORIGINALS
            / f"{dt.year:04d}"
            / f"{dt.month:02d}"
            / f"{dt.day:02d}"
            / f"{dt.strftime('%Y%m%d_%H%M%S')}_{file_hash[:8]}{path.suffix.lower()}"
        )
        dest.parent.mkdir(parents=True, exist_ok=True)

        shutil.copy2(path, dest)
        write_xmp(dest, batch_id, camera_model)
        insert_photo(
            {
                "hash": file_hash,
                "original_filename": path.name,
                "imported_path": str(dest),
                "camera_model": camera_model,
                "capture_date": capture_date,
                "import_timestamp": datetime.now().isoformat(),
                "status": "raw",
            },
            db_path=db_path,
        )
        path.unlink()
        return "imported"

    except Exception as exc:
        log("INGEST", f"FAILED {path.name}: {exc}")
        return "failed"


def run_ingest(inbox: Path = INBOX, db_path: Path = DB_PATH) -> dict[str, int]:
    """Scan inbox for supported photos, import each, then git commit. Returns counts dict."""
    init_db(db_path=db_path)
    batch_id = datetime.now().isoformat()

    imported = skipped = failed = 0
    for path in sorted(inbox.rglob("*")):
        if not path.is_file():
            continue
        if path.suffix.lower() not in SUPPORTED_EXTENSIONS:
            continue
        result = import_file(path, batch_id, db_path=db_path)
        if result == "imported":
            imported += 1
        elif result == "skipped":
            skipped += 1
        else:
            failed += 1

    log("INGEST", f"Done: {imported} imported, {skipped} skipped, {failed} failed")
    msg = f"ingest: {batch_id[:10]} ({imported} photos)"
    if git_commit(msg):
        git_push()
    return {"imported": imported, "skipped": skipped, "failed": failed}


if __name__ == "__main__":
    run_ingest()

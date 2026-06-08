import sqlite3
from pathlib import Path

from config import DB_PATH

_SCHEMA = """
CREATE TABLE IF NOT EXISTS photos (
    hash TEXT PRIMARY KEY,
    original_filename TEXT,
    imported_path TEXT,
    camera_model TEXT,
    source_device TEXT,
    capture_date TEXT,
    import_timestamp TEXT,
    status TEXT DEFAULT 'raw'
);
"""

_COLUMNS = {
    "hash", "original_filename", "imported_path",
    "camera_model", "source_device", "capture_date",
    "import_timestamp", "status",
}


def init_db(db_path: Path = DB_PATH) -> None:
    """Create the photos table if it does not already exist."""
    db_path.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(db_path) as conn:
        conn.execute(_SCHEMA)


def hash_exists(hash: str, db_path: Path = DB_PATH) -> bool:
    """Return True if a photo with this SHA-256 hash is already in the catalog."""
    with sqlite3.connect(db_path) as conn:
        row = conn.execute("SELECT 1 FROM photos WHERE hash = ?", (hash,)).fetchone()
    return row is not None


def insert_photo(record: dict, db_path: Path = DB_PATH) -> None:
    """Insert a photo record into the catalog. Unknown keys in record are ignored."""
    cols = [k for k in record if k in _COLUMNS]
    placeholders = ", ".join(f":{c}" for c in cols)
    col_list = ", ".join(cols)
    with sqlite3.connect(db_path) as conn:
        conn.execute(
            f"INSERT INTO photos ({col_list}) VALUES ({placeholders})",
            {c: record[c] for c in cols},
        )


def update_status(hash: str, status: str, db_path: Path = DB_PATH) -> None:
    """Update the status of a photo. Valid values: raw, culled, edited, published."""
    with sqlite3.connect(db_path) as conn:
        conn.execute("UPDATE photos SET status = ? WHERE hash = ?", (status, hash))

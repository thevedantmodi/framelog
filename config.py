from datetime import datetime
from pathlib import Path

INBOX = Path("~/Photos/inbox").expanduser()
ORIGINALS = Path("~/Photos/originals").expanduser()
PROCESSED = Path("~/Photos/processed").expanduser()
DB_PATH = Path("~/Photos/catalog.db").expanduser()
SUPPORTED_EXTENSIONS = {".raf", ".cr3", ".dng", ".heic", ".jpg", ".jpeg", ".mp4", ".mov"}
DEBOUNCE_SECONDS = 10


def log(prefix: str, msg: str) -> None:
    ts = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"{ts} [{prefix}] {msg}", flush=True)

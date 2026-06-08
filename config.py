from pathlib import Path

INBOX = Path("~/Photos/inbox").expanduser()
ORIGINALS = Path("~/Photos/originals").expanduser()
PROCESSED = Path("~/Photos/processed").expanduser()
DB_PATH = Path("~/Photos/catalog.db").expanduser()
SUPPORTED_EXTENSIONS = {".raf", ".heic", ".jpg", ".jpeg", ".mp4", ".mov"}
DEBOUNCE_SECONDS = 10

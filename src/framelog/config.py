import logging
from pathlib import Path

FRAMELOG_DIR = Path("~/.framelog").expanduser()

INBOX = Path("~/Photos/inbox").expanduser()
ORIGINALS = Path("~/Photos/originals").expanduser()
PROCESSED = Path("~/Photos/processed").expanduser()
DB_PATH = FRAMELOG_DIR / "catalog.db"
SUPPORTED_EXTENSIONS = {
    ".raf",
    ".cr3",
    ".dng",
    ".heic",
    ".jpg",
    ".jpeg",
    ".mp4",
    ".mov",
}
DEBOUNCE_SECONDS = 10
SD_PAUSE_FLAG = FRAMELOG_DIR / "sd_paused"
LOG_FILE = FRAMELOG_DIR / "framelog.log"
INGEST_TRIGGER = FRAMELOG_DIR / "ingest_trigger"


def setup_logging() -> None:
    """Configure the framelog logger. Call once at process startup."""
    FRAMELOG_DIR.mkdir(parents=True, exist_ok=True)
    logger = logging.getLogger("framelog")
    if logger.handlers:
        return
    logger.setLevel(logging.DEBUG)
    handler = logging.FileHandler(LOG_FILE, encoding="utf-8")
    handler.setFormatter(logging.Formatter(
        "%(asctime)s [%(name)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    ))
    logger.addHandler(handler)


def log(prefix: str, msg: str) -> None:
    logging.getLogger("framelog").info(msg)

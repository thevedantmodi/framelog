import logging
import threading
import time
from datetime import datetime
from pathlib import Path

import psutil
from watchdog.events import FileSystemEventHandler
from watchdog.observers import Observer

from config import DEBOUNCE_SECONDS, ORIGINALS
from git import git_commit, git_push

logging.basicConfig(
    filename=Path("~/Photos/framelog.log").expanduser(),
    level=logging.INFO,
    format="%(asctime)s %(message)s",
)

LIGHTROOM_PROCESS = "Adobe Lightroom Classic"


def _is_lightroom_running() -> bool:
    return any(
        LIGHTROOM_PROCESS in (p.name() or "") for p in psutil.process_iter(["name"])
    )


def _on_ac_power() -> bool:
    import subprocess

    result = subprocess.run(["pmset", "-g", "batt"], capture_output=True, text=True)
    return "AC Power" in result.stdout


class XMPHandler(FileSystemEventHandler):
    def __init__(self):
        self._changed: set[str] = set()
        self._timer: threading.Timer | None = None
        self._lock = threading.Lock()

    def on_modified(self, event):
        """Accumulate changed XMP paths and reset the debounce timer on each event."""
        if event.is_directory:
            return
        if not event.src_path.endswith(".xmp"):
            return
        with self._lock:
            self._changed.add(event.src_path)
            if self._timer:
                self._timer.cancel()
            self._timer = threading.Timer(DEBOUNCE_SECONDS, self._commit)
            self._timer.start()

    def _commit(self):
        with self._lock:
            files = list(self._changed)
            self._changed.clear()
            self._timer = None
        if not files:
            return
        now = datetime.now().strftime("%Y-%m-%d %H:%M")
        msg = f"auto: xmp changes {now} ({len(files)} files)"
        committed = git_commit(msg)
        if committed:
            logging.info(msg)
            if _on_ac_power():
                git_push()

    def flush(self):
        """Cancel any pending timer and commit immediately. Called when Lightroom quits."""
        with self._lock:
            if self._timer:
                self._timer.cancel()
                self._timer = None
        self._commit()


def run_watchdog(originals: Path = ORIGINALS):
    """Watch originals/ for XMP changes while Lightroom is running. Blocks until interrupted."""
    logging.info("watchdog started")
    handler = XMPHandler()
    observer = Observer()
    observer.schedule(handler, str(originals), recursive=True)
    observer.start()

    try:
        was_running = False
        while True:
            running = _is_lightroom_running()
            if running and not was_running:
                logging.info("Lightroom opened — watching for XMP changes")
            if not running and was_running:
                logging.info("Lightroom closed — flushing final commit")
                handler.flush()
                git_push()
            was_running = running
            time.sleep(5)
    except KeyboardInterrupt:
        pass
    finally:
        observer.stop()
        observer.join()
        logging.info("watchdog stopped")


if __name__ == "__main__":
    run_watchdog()

import sqlite3
import subprocess
import threading
import time
from datetime import datetime
from pathlib import Path

import psutil
import rumps
from watchdog.events import FileSystemEventHandler
from watchdog.observers import Observer

from config import DB_PATH, DEBOUNCE_SECONDS, ORIGINALS, PROCESSED, SUPPORTED_EXTENSIONS
from git import git_commit, git_push
from ingest import run_ingest
from outgest import run_outgest

LOG_PATH = Path("~/Photos/framelog.log").expanduser()
INGEST_TRIGGER = Path("~/Photos/.ingest_trigger").expanduser()
LIGHTROOM_PROCESS = "Adobe Lightroom Classic"


# --- helpers -----------------------------------------------------------------


def _notify(title: str, message: str) -> None:
    subprocess.run(
        [
            "osascript",
            "-e",
            f'display notification "{message}" with title "{title}"',
        ]
    )


def _photo_count() -> int:
    if not DB_PATH.exists():
        return 0
    with sqlite3.connect(DB_PATH) as conn:
        row = conn.execute("SELECT COUNT(*) FROM photos").fetchone()
    return row[0] if row else 0


def _last_import() -> str:
    if not DB_PATH.exists():
        return "never"
    with sqlite3.connect(DB_PATH) as conn:
        row = conn.execute(
            "SELECT import_timestamp FROM photos ORDER BY import_timestamp DESC LIMIT 1"
        ).fetchone()
    if not row:
        return "never"
    return row[0][:16].replace("T", " ")


def _tail_log(n: int = 8) -> str:
    if not LOG_PATH.exists():
        return "no log yet"
    lines = LOG_PATH.read_text().splitlines()
    return "\n".join(lines[-n:]) if lines else "empty"


def _on_ac_power() -> bool:
    result = subprocess.run(["pmset", "-g", "batt"], capture_output=True, text=True)
    return "AC Power" in result.stdout


def _is_lightroom_running() -> bool:
    return any(
        LIGHTROOM_PROCESS in (p.name() or "") for p in psutil.process_iter(["name"])
    )


# --- XMP watcher -------------------------------------------------------------


class XMPHandler(FileSystemEventHandler):
    def __init__(self):
        self._changed: set[str] = set()
        self._timer: threading.Timer | None = None
        self._lock = threading.Lock()

    def on_modified(self, event):  # type: ignore[override]
        if event.is_directory:
            return
        src = (
            event.src_path
            if isinstance(event.src_path, str)
            else event.src_path.decode()
        )
        if not any(
            src.lower().endswith(ext) for ext in (".xmp", ".jpg", ".jpeg", ".heic")
        ):
            return
        with self._lock:
            self._changed.add(src)
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
        if git_commit(msg) and _on_ac_power():
            git_push()

    def flush(self):
        with self._lock:
            if self._timer:
                self._timer.cancel()
                self._timer = None
        self._commit()


def _run_xmp_watcher():
    handler = XMPHandler()
    observer = Observer()
    observer.schedule(handler, str(ORIGINALS), recursive=True)
    observer.start()
    was_running = False
    while True:
        running = _is_lightroom_running()
        if not running and was_running:
            handler.flush()
            git_push()
        was_running = running
        time.sleep(5)


# --- outgest watcher ---------------------------------------------------------


class OutgestHandler(FileSystemEventHandler):
    def __init__(self):
        self._timer: threading.Timer | None = None
        self._lock = threading.Lock()

    def on_created(self, event):  # type: ignore[override]
        if event.is_directory:
            return
        src = (
            event.src_path
            if isinstance(event.src_path, str)
            else event.src_path.decode()
        )
        if Path(src).suffix.lower() not in SUPPORTED_EXTENSIONS:
            return
        with self._lock:
            if self._timer:
                self._timer.cancel()
            self._timer = threading.Timer(3, self._run)
            self._timer.start()

    def _run(self):
        with self._lock:
            self._timer = None
        counts = run_outgest()
        if counts["moved"] > 0:
            _notify("Framelog — Outgest", f"{counts['moved']} files organized")


def _run_outgest_watcher():
    PROCESSED.mkdir(parents=True, exist_ok=True)
    handler = OutgestHandler()
    observer = Observer()
    observer.schedule(handler, str(PROCESSED), recursive=False)
    observer.start()
    observer.join()


# --- ingest trigger poller ---------------------------------------------------


def _run_ingest_poller(app: "FramelogApp"):
    while True:
        if INGEST_TRIGGER.exists():
            try:
                INGEST_TRIGGER.unlink()
            except FileNotFoundError:
                pass
            app._do_ingest(triggered_by="sd_card")
        time.sleep(3)


# --- menu bar app ------------------------------------------------------------


class FramelogApp(rumps.App):
    def __init__(self):
        super().__init__("📷", quit_button=None)  # pyright: ignore[reportArgumentType]
        self._ingest_running = False
        self._build_menu()
        self._start_background_threads()

    def _build_menu(self):
        self.menu = [
            rumps.MenuItem("Status", callback=None),
            None,
            rumps.MenuItem("Run Ingest Now", callback=self.run_ingest),
            rumps.MenuItem("Run Outgest Now", callback=self.run_outgest),
            None,
            rumps.MenuItem("Open Log File", callback=self.open_log),
            None,
            rumps.MenuItem("Quit Framelog", callback=rumps.quit_application),
        ]
        self._refresh_status()

    def _start_background_threads(self):
        for target in (_run_xmp_watcher, _run_outgest_watcher):
            threading.Thread(target=target, daemon=True).start()
        threading.Thread(target=_run_ingest_poller, args=(self,), daemon=True).start()

    def _refresh_status(self):
        count = _photo_count()
        last = _last_import()
        self.menu["Status"].title = f"{count} photos · last import {last}"
        self.title = "⏳" if self._ingest_running else "📷"

    @rumps.timer(30)
    def tick(self, _):
        self._refresh_status()

    def _do_ingest(self, triggered_by: str = "manual"):
        if self._ingest_running:
            return
        self._ingest_running = True
        self.title = "⏳"

        def _run():
            try:
                counts = run_ingest()
                _notify(
                    "Framelog — Ingest complete",
                    f"{counts['imported']} imported · {counts['skipped']} skipped · {counts['failed']} failed",
                )
            except Exception as exc:
                _notify("Framelog — Ingest failed", str(exc))
            finally:
                self._ingest_running = False
                self._refresh_status()

        threading.Thread(target=_run, daemon=True).start()

    def run_ingest(self, _):
        if self._ingest_running:
            _notify("Framelog", "Ingest already running")
            return
        self._do_ingest()

    def run_outgest(self, _):
        def _run():
            try:
                counts = run_outgest()
                _notify(
                    "Framelog — Outgest complete",
                    f"{counts['moved']} moved · {counts['skipped']} skipped · {counts['failed']} failed",
                )
            except Exception as exc:
                _notify("Framelog — Outgest failed", str(exc))

        threading.Thread(target=_run, daemon=True).start()

    def open_log(self, _):
        subprocess.run(["open", str(LOG_PATH)])


if __name__ == "__main__":
    FramelogApp().run()

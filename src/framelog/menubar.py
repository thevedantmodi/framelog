import logging
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

from framelog.config import DB_PATH, DEBOUNCE_SECONDS, INGEST_TRIGGER, LOG_FILE, ORIGINALS, PROCESSED, SD_PAUSE_FLAG, SUPPORTED_EXTENSIONS

_log = logging.getLogger("framelog.menubar")
from framelog.firstrun import git_has_remote, run_setup, set_git_remote, setup_needed
from framelog.git import git_commit, git_push
from framelog.ingest import run_ingest
from framelog.outgest import run_outgest

LOG_PATH = LOG_FILE
LIGHTROOM_PROCESS = "Adobe Lightroom Classic"


# --- helpers -----------------------------------------------------------------


def _notify(title: str, message: str) -> None:
    rumps.notification(title, "", message)


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
        try:
            committed = git_commit(msg)
            if committed:
                _log.info("committed: %s", msg)
                if _on_ac_power():
                    git_push()
                    _log.info("pushed")
            else:
                _log.debug("nothing to commit")
        except Exception:
            _log.exception("git commit failed")

    def flush(self):
        with self._lock:
            if self._timer:
                self._timer.cancel()
                self._timer = None
        self._commit()


def _run_xmp_watcher():
    if not ORIGINALS.exists():
        _log.warning("originals/ does not exist — XMP watcher not started")
        return
    _log.info("XMP watcher started on %s", ORIGINALS)
    handler = XMPHandler()
    observer = Observer()
    observer.schedule(handler, str(ORIGINALS), recursive=True)
    observer.start()
    was_running = False
    try:
        while True:
            running = _is_lightroom_running()
            if running and not was_running:
                _log.info("Lightroom opened")
            if not running and was_running:
                _log.info("Lightroom closed — flushing")
                handler.flush()
                try:
                    git_push()
                except Exception:
                    _log.exception("git push failed after Lightroom close")
            was_running = running
            time.sleep(5)
    except Exception:
        _log.exception("XMP watcher crashed")
    finally:
        observer.stop()
        observer.join()


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
        try:
            counts = run_outgest()
            _log.info("outgest: %s moved, %s skipped, %s failed",
                      counts["moved"], counts["skipped"], counts["failed"])
            if counts["moved"] > 0:
                _notify("Framelog — Outgest", f"{counts['moved']} files organized")
        except Exception:
            _log.exception("outgest failed")


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
            if not SD_PAUSE_FLAG.exists():
                app._do_ingest(triggered_by="sd_card")
        time.sleep(3)


# --- menu bar app ------------------------------------------------------------


class FramelogApp(rumps.App):
    def __init__(self):
        super().__init__("📷", quit_button=None)  # pyright: ignore[reportArgumentType]
        self._ingest_running = False
        self._build_menu()
        self._start_background_threads()
        if setup_needed():
            threading.Thread(target=self._first_run_setup, daemon=True).start()

    def _build_menu(self):
        self._sd_toggle = rumps.MenuItem(self._sd_toggle_label(), callback=self.toggle_sd_watcher)
        self.menu = [
            rumps.MenuItem("Status", callback=None),
            None,
            rumps.MenuItem("Run Ingest Now", callback=self.run_ingest),
            rumps.MenuItem("Run Outgest Now", callback=self.run_outgest),
            self._sd_toggle,
            None,
            rumps.MenuItem("Open Log File", callback=self.open_log),
            None,
            rumps.MenuItem("Set Git Remote…", callback=self.set_git_remote),
            rumps.MenuItem("Run Setup", callback=self.run_setup),
            rumps.MenuItem("Quit Framelog", callback=rumps.quit_application),
        ]
        self._refresh_status()

    def _first_run_setup(self) -> None:
        try:
            run_setup()
            if git_has_remote():
                _notify("Framelog", "Setup complete — ready to import photos.")
            else:
                _notify("Framelog", "Setup complete — add a Git remote via the menu to enable XMP versioning.")
        except Exception as exc:
            _notify("Framelog — Setup failed", str(exc))

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

    def _sd_toggle_label(self) -> str:
        return "Resume SD Watcher" if SD_PAUSE_FLAG.exists() else "Pause SD Watcher"

    def toggle_sd_watcher(self, _):
        if SD_PAUSE_FLAG.exists():
            SD_PAUSE_FLAG.unlink()
            _notify("Framelog", "SD watcher resumed.")
        else:
            SD_PAUSE_FLAG.touch()
            _notify("Framelog", "SD watcher paused — card inserts will be ignored.")
        self._sd_toggle.title = self._sd_toggle_label()

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

    def set_git_remote(self, _):
        current = ""
        try:
            result = subprocess.run(
                ["git", "-C", str(ORIGINALS), "remote", "get-url", "origin"],
                capture_output=True, text=True,
            )
            if result.returncode == 0:
                current = result.stdout.strip()
        except Exception:
            pass
        window = rumps.Window(
            message="GitHub SSH URL for XMP version history:",
            title="Framelog — Git Remote",
            default_text=current or "git@github.com:you/framelog-xmp.git",
            ok="Save",
            cancel="Cancel",
            dimensions=(400, 20),
        )
        response = window.run()
        if response.clicked and response.text.strip():
            try:
                set_git_remote(response.text.strip())
                _notify("Framelog", "Git remote configured.")
            except Exception as exc:
                _notify("Framelog — Git setup failed", str(exc))

    def open_log(self, _):
        subprocess.run(["open", str(LOG_PATH)])

    def run_setup(self, _):
        threading.Thread(target=self._first_run_setup, daemon=True).start()


if __name__ == "__main__":
    FramelogApp().run()

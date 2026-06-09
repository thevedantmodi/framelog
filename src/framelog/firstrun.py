import os
import stat
import subprocess
import sys
from pathlib import Path

from framelog.config import INBOX, ORIGINALS, PROCESSED

LAUNCH_AGENTS = Path("~/Library/LaunchAgents").expanduser()
SETUP_MARKER = Path("~/.framelog_setup_done").expanduser()


def _bundle_dir() -> Path:
    """Return the directory containing non-Python resources."""
    bundle = _app_bundle()
    if bundle:
        return bundle / "Contents" / "Resources"
    return Path(__file__).parent


def _app_bundle() -> Path | None:
    """Return the .app bundle root if running from a py2app bundle, else None."""
    exe = Path(sys.executable)
    # py2app: .app/Contents/MacOS/python -> .app/Contents -> .app
    if "Contents/MacOS" in str(exe):
        return exe.parent.parent.parent
    return None


def setup_needed() -> bool:
    return not SETUP_MARKER.exists()


def run_setup() -> None:
    """Create directories, install launchd agents, mark setup done."""
    for d in (INBOX, ORIGINALS, PROCESSED):
        d.mkdir(parents=True, exist_ok=True)

    _install_sdcard_plist()
    SETUP_MARKER.touch()


def _install_sdcard_plist() -> None:
    script_src = _bundle_dir() / "on_sd_mount.sh"
    script_dst = Path("~/.framelog_on_sd_mount.sh").expanduser()

    script_dst.write_text(script_src.read_text(encoding="utf-8"), encoding="utf-8")
    script_dst.chmod(script_dst.stat().st_mode | stat.S_IEXEC)

    plist = _sdcard_plist(str(script_dst))
    plist_path = LAUNCH_AGENTS / "com.framelog.sdcard.plist"
    LAUNCH_AGENTS.mkdir(parents=True, exist_ok=True)
    plist_path.write_text(plist)

    _ = subprocess.run(["launchctl", "unload", str(plist_path)], capture_output=True)
    subprocess.run(["launchctl", "load", str(plist_path)], check=True)


def _sdcard_plist(script_path: str) -> str:
    log = str(Path("~/Photos/framelog.log").expanduser())
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.framelog.sdcard</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>{script_path}</string>
    </array>
    <key>WatchPaths</key>
    <array>
        <string>/Volumes</string>
    </array>
    <key>StandardOutPath</key>
    <string>{log}</string>
    <key>StandardErrorPath</key>
    <string>{log}</string>
    <key>RunAtLoad</key>
    <false/>
</dict>
</plist>
"""

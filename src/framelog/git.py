import shutil
import subprocess
from pathlib import Path

from framelog.config import ORIGINALS

_GIT_CANDIDATES = [
    "/usr/bin/git",
    "/opt/homebrew/bin/git",
    "/usr/local/bin/git",
]


def _find_git() -> str:
    for candidate in _GIT_CANDIDATES:
        if Path(candidate).exists():
            return candidate
    found = shutil.which("git")
    if found:
        return found
    raise RuntimeError(
        "git not found. Install Xcode Command Line Tools: xcode-select --install"
    )


def git_commit(message: str, originals: Path = ORIGINALS) -> bool:
    """Stage all changes in originals/ and commit. Returns False if nothing to commit."""
    git = _find_git()
    _ = subprocess.run([git, "-C", str(originals), "add", "-A"], check=True)
    status = subprocess.run(
        [git, "-C", str(originals), "status", "--porcelain"],
        capture_output=True,
        text=True,
        check=True,
    )
    if not status.stdout.strip():
        return False
    _ = subprocess.run([git, "-C", str(originals), "commit", "-m", message], check=True)
    return True


def git_push(originals: Path = ORIGINALS) -> bool:
    """Push originals/ to remote. Skips and returns False if on battery power."""
    result = subprocess.run(["pmset", "-g", "batt"], capture_output=True, text=True)
    if "AC Power" not in result.stdout:
        return False
    git = _find_git()
    result = subprocess.run(
        [git, "-C", str(originals), "push", "--force-with-lease", "origin", "main"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"git push failed: {result.stderr.strip()}")
    return True

import subprocess
from pathlib import Path

from config import ORIGINALS


def git_commit(message: str, originals: Path = ORIGINALS) -> bool:
    """Stage all changes in originals/ and commit. Returns False if nothing to commit."""
    _ = subprocess.run(["git", "-C", str(originals), "add", "-A"], check=True)
    status = subprocess.run(
        ["git", "-C", str(originals), "status", "--porcelain"],
        capture_output=True, text=True, check=True,
    )
    if not status.stdout.strip():
        return False
    _ = subprocess.run(["git", "-C", str(originals), "commit", "-m", message], check=True)
    return True


def git_push(originals: Path = ORIGINALS) -> bool:
    """Push originals/ to remote. Skips and returns False if on battery power."""
    result = subprocess.run(["pmset", "-g", "batt"], capture_output=True, text=True)
    if "AC Power" not in result.stdout:
        return False
    _ = subprocess.run(["git", "-C", str(originals), "push"], check=True)
    return True

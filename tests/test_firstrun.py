import subprocess
import sys
from pathlib import Path
from unittest.mock import call, patch

import pytest

from framelog.firstrun import _init_git_repo, _install_app_plist, git_has_remote, set_git_remote


@pytest.fixture
def git_dir(tmp_path):
    """A tmp_path with a bare git repo already initialized."""
    subprocess.run(["git", "init", str(tmp_path)], check=True, capture_output=True)
    return tmp_path


def test_init_git_repo_creates_repo(tmp_path):
    with patch("framelog.firstrun.ORIGINALS", tmp_path):
        _init_git_repo()
    assert (tmp_path / ".git").exists()


def test_init_git_repo_writes_gitignore(tmp_path):
    with patch("framelog.firstrun.ORIGINALS", tmp_path):
        _init_git_repo()
    content = (tmp_path / ".gitignore").read_text()
    assert "!*.xmp" in content
    assert "!.gitignore" in content


def test_init_git_repo_idempotent(tmp_path):
    with patch("framelog.firstrun.ORIGINALS", tmp_path):
        _init_git_repo()
        _init_git_repo()  # second call must not raise or clobber
    assert (tmp_path / ".git").exists()


def test_init_git_repo_preserves_existing_gitignore(tmp_path):
    with patch("framelog.firstrun.ORIGINALS", tmp_path):
        _init_git_repo()
        (tmp_path / ".gitignore").write_text("custom content")
        _init_git_repo()
    assert (tmp_path / ".gitignore").read_text() == "custom content"


def test_git_has_remote_false_on_fresh_repo(git_dir):
    with patch("framelog.firstrun.ORIGINALS", git_dir):
        assert git_has_remote() is False


def test_git_has_remote_true_after_add(git_dir):
    subprocess.run(
        ["git", "-C", str(git_dir), "remote", "add", "origin", "git@github.com:x/y.git"],
        check=True,
    )
    with patch("framelog.firstrun.ORIGINALS", git_dir):
        assert git_has_remote() is True


def test_set_git_remote_adds_when_none(git_dir):
    with patch("framelog.firstrun.ORIGINALS", git_dir):
        set_git_remote("git@github.com:x/y.git")
        assert git_has_remote() is True

    result = subprocess.run(
        ["git", "-C", str(git_dir), "remote", "get-url", "origin"],
        capture_output=True, text=True,
    )
    assert result.stdout.strip() == "git@github.com:x/y.git"


def test_install_app_plist_never_loads(tmp_path):
    fake_bundle = tmp_path / "Framelog.app" / "Contents" / "MacOS" / "Framelog"
    fake_bundle.parent.mkdir(parents=True)
    fake_bundle.touch()
    fake_exe = str(fake_bundle.parent / "python")

    with patch("framelog.firstrun.LAUNCH_AGENTS", tmp_path), \
         patch("sys.executable", fake_exe), \
         patch("subprocess.run") as mock_run:
        _install_app_plist()

    called_args = [c.args[0] for c in mock_run.call_args_list]
    assert not any("load" in str(a) for a in called_args), \
        "launchctl load must not be called — would spawn a second instance"
    plist_path = tmp_path / "com.framelog.app.plist"
    assert plist_path.exists()


def test_set_git_remote_updates_existing(git_dir):
    subprocess.run(
        ["git", "-C", str(git_dir), "remote", "add", "origin", "git@github.com:old/repo.git"],
        check=True,
    )
    with patch("framelog.firstrun.ORIGINALS", git_dir):
        set_git_remote("git@github.com:new/repo.git")

    result = subprocess.run(
        ["git", "-C", str(git_dir), "remote", "get-url", "origin"],
        capture_output=True, text=True,
    )
    assert result.stdout.strip() == "git@github.com:new/repo.git"

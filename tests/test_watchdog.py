import threading
import time
from pathlib import Path
from unittest.mock import MagicMock, call, patch

import pytest

from watcher import XMPHandler, _is_lightroom_running, _on_ac_power


def test_is_lightroom_running_true():
    mock_proc = MagicMock()
    mock_proc.name.return_value = "Adobe Lightroom Classic"
    with patch("psutil.process_iter", return_value=[mock_proc]):
        assert _is_lightroom_running() is True


def test_is_lightroom_running_false():
    mock_proc = MagicMock()
    mock_proc.name.return_value = "Finder"
    with patch("psutil.process_iter", return_value=[mock_proc]):
        assert _is_lightroom_running() is False


def test_on_ac_power_true():
    with patch("subprocess.run") as mock_run:
        mock_run.return_value = MagicMock(stdout="Now drawing from 'AC Power'")
        assert _on_ac_power() is True


def test_on_ac_power_false():
    with patch("subprocess.run") as mock_run:
        mock_run.return_value = MagicMock(stdout="Now drawing from 'Battery Power'")
        assert _on_ac_power() is False


def test_xmp_handler_debounce(tmp_path):
    handler = XMPHandler()
    event = MagicMock()
    event.is_directory = False
    event.src_path = str(tmp_path / "photo.xmp")

    with patch("watcher.git_commit", return_value=True) as mock_commit, \
         patch("watcher.git_push") as mock_push, \
         patch("watcher._on_ac_power", return_value=False):
        handler.on_modified(event)
        handler.on_modified(event)  # second event resets timer
        # flush immediately instead of waiting for debounce
        handler.flush()

    mock_commit.assert_called_once()
    msg = mock_commit.call_args[0][0]
    assert "auto: xmp changes" in msg
    assert "1 files" in msg


def test_xmp_handler_ignores_non_xmp(tmp_path):
    handler = XMPHandler()
    event = MagicMock()
    event.is_directory = False
    event.src_path = str(tmp_path / "photo.raf")

    handler.on_modified(event)
    assert len(handler._changed) == 0


def test_xmp_handler_ignores_directories():
    handler = XMPHandler()
    event = MagicMock()
    event.is_directory = True
    event.src_path = "/some/dir.xmp"

    handler.on_modified(event)
    assert len(handler._changed) == 0


def test_flush_pushes_on_ac(tmp_path):
    handler = XMPHandler()
    handler._changed.add(str(tmp_path / "photo.xmp"))

    with patch("watcher.git_commit", return_value=True), \
         patch("watcher.git_push") as mock_push, \
         patch("watcher._on_ac_power", return_value=True):
        handler.flush()

    mock_push.assert_called_once()


def test_flush_no_push_on_battery(tmp_path):
    handler = XMPHandler()
    handler._changed.add(str(tmp_path / "photo.xmp"))

    with patch("watcher.git_commit", return_value=True), \
         patch("watcher.git_push") as mock_push, \
         patch("watcher._on_ac_power", return_value=False):
        handler.flush()

    mock_push.assert_not_called()

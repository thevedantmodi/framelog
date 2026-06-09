from unittest.mock import MagicMock, call, patch

import pytest

from framelog.git import git_commit, git_push


def _run(stdout="", returncode=0):
    m = MagicMock()
    m.stdout = stdout
    m.returncode = returncode
    return m


def test_commit_with_changes(tmp_path):
    with patch("subprocess.run") as mock_run:
        mock_run.side_effect = [
            _run(),                      # add -A
            _run("M originals/foo.xmp"), # status --porcelain (has changes)
            _run(),                      # commit
        ]
        result = git_commit("ingest: test", originals=tmp_path)
    assert result is True
    assert mock_run.call_count == 3


def test_commit_no_changes(tmp_path):
    with patch("subprocess.run") as mock_run:
        mock_run.side_effect = [
            _run(),   # add -A
            _run(""), # status --porcelain (empty = no changes)
        ]
        result = git_commit("ingest: test", originals=tmp_path)
    assert result is False
    assert mock_run.call_count == 2


def test_push_on_ac_power(tmp_path):
    with patch("subprocess.run") as mock_run:
        mock_run.side_effect = [
            _run("Now drawing from 'AC Power'"), # pmset
            _run(),                               # git push
        ]
        result = git_push(originals=tmp_path)
    assert result is True
    assert mock_run.call_count == 2


def test_push_on_battery(tmp_path):
    with patch("subprocess.run") as mock_run:
        mock_run.side_effect = [
            _run("Now drawing from 'Battery Power'"), # pmset
        ]
        result = git_push(originals=tmp_path)
    assert result is False
    assert mock_run.call_count == 1

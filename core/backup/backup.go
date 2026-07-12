// Package backup syncs originals/ to an external backup drive via rclone.
// It uses `rclone copy` (not sync) per PROTOCOL.md §6: a bad deletion on the
// source side must never propagate to the backup. An unplugged or unmounted
// backup drive is the most common real-world state — Sync returns (false, nil)
// in that case rather than failing, so callers are never penalised for running
// without an attached drive.
//
// The injectable-binary-path pattern from core/gitops and core/exif applies
// here too: FindRclone returns the path, Sync takes it as a parameter. Tests
// can pass a fake shell script — no real rclone required.
package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// rcloneCandidates is the ordered list of known rclone binary locations.
// Package-level var (not const) so FindRclone tests can force the LookPath branch.
var rcloneCandidates = []string{
	"/opt/homebrew/bin/rclone",
	"/usr/local/bin/rclone",
}

// FindRclone returns the absolute path to the rclone binary. Checks known
// install locations first, then falls back to exec.LookPath. Returns an
// actionable error if neither resolves.
func FindRclone() (string, error) {
	for _, p := range rcloneCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("rclone"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("rclone not found. Install it with: brew install rclone")
}

// IsDriveMounted reports whether backupPath exists and is a directory.
// Returns false without error when the drive is absent — an unmounted backup
// drive is the expected normal state between ingest runs, not a failure.
// Used by Sync internally and by the IPC status handler (FL-302) to populate
// the backup_drive_mounted field in the status response.
func IsDriveMounted(backupPath string) bool {
	fi, err := os.Stat(backupPath)
	return err == nil && fi.IsDir()
}

// rcloneLogEntry is one line of rclone's --use-json-log output.
type rcloneLogEntry struct {
	Msg    string `json:"msg"`
	Object string `json:"object"`
}

// Sync copies originalsPath into backupPath/originals/ using rclone copy.
// Returns (false, nil) without invoking rclone when backupPath does not exist
// or is not a directory — an unmounted backup drive is expected, not an error.
// Returns (false, err) when rclone exits non-zero; err wraps stderr output.
//
// The optional onCopy callback is invoked after each file rclone reports
// copied, with the file's base name and the running copied-so-far count,
// parsed from rclone's JSON log stream. Pass nil (or omit) when progress
// reporting is not needed.
func Sync(rclonePath, originalsPath, backupPath string, onCopy ...func(filename string, n int)) (bool, error) {
	if !IsDriveMounted(backupPath) {
		return false, nil
	}

	var cb func(string, int)
	if len(onCopy) > 0 {
		cb = onCopy[0]
	}

	dest := filepath.Join(backupPath, "originals")

	cmd := exec.Command(rclonePath, "copy", originalsPath, dest, "--use-json-log", "-v")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("rclone copy: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("rclone copy: %w", err)
	}

	var count int
	var stderrBuf bytes.Buffer
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Bytes()
		stderrBuf.Write(line)
		stderrBuf.WriteByte('\n')

		var entry rcloneLogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // non-JSON noise line
		}
		// "Copied (new)" for a normal transfer, "Copied (server-side copy)"
		// when src/dst share a filesystem — match both, but not "Copied
		// (replaced existing)" since that isn't a fresh copy.
		if strings.HasPrefix(entry.Msg, "Copied (") && !strings.Contains(entry.Msg, "replaced existing") {
			count++
			if cb != nil {
				cb(filepath.Base(entry.Object), count)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return false, fmt.Errorf("rclone copy: %w; stderr: %s",
			err, bytes.TrimSpace(stderrBuf.Bytes()))
	}
	return true, nil
}

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
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Sync copies originalsPath into backupPath/originals/ using rclone copy.
// Returns (false, nil) without invoking rclone when backupPath does not exist
// or is not a directory — an unmounted backup drive is expected, not an error.
// Returns (false, err) when rclone exits non-zero; err wraps stderr output.
func Sync(rclonePath, originalsPath, backupPath string) (bool, error) {
	fi, err := os.Stat(backupPath)
	if err != nil || !fi.IsDir() {
		return false, nil
	}

	dest := filepath.Join(backupPath, "originals")

	var stderr bytes.Buffer
	cmd := exec.Command(rclonePath, "copy", originalsPath, dest)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("rclone copy: %w; stderr: %s",
			err, bytes.TrimSpace(stderr.Bytes()))
	}
	return true, nil
}

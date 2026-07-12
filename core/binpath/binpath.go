// Package binpath resolves external binary paths, shared by every package
// that shells out to a macOS command (git, exiftool, rclone, diskutil,
// pmset, pgrep, launchctl). Each caller keeps its own package-level
// candidates slice (so tests can force the exec.LookPath fallback branch)
// and its own not-found error text; only the stat-then-lookup loop is
// shared here.
package binpath

import (
	"os"
	"os/exec"
)

// Find checks candidates in order for a file that stats successfully, then
// falls back to exec.LookPath(lookupName). Returns notFound unchanged if
// neither resolves.
func Find(candidates []string, lookupName string, notFound error) (string, error) {
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(lookupName); err == nil {
		return p, nil
	}
	return "", notFound
}

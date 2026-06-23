// Package sdcard detects SD cards mounted under /Volumes, copies their DCIM
// contents into inbox/, and fires RunIngest. It ports the detection logic from
// on_sd_mount.sh — the dual check (diskutil says removable AND a DCIM directory
// is present) naturally excludes other things that mount under /Volumes (backup
// drives, network shares) without any explicit special-casing.
//
// The injectable-binary-path pattern from core/exif and core/gitops is applied
// here for diskutil: FindDiskutil returns the path, IsRemovableMedia takes it
// as a parameter. Tests can therefore pass a fake shell script with no real
// diskutil present — which matters because diskutil only exists on macOS.
package sdcard

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
)

// IngestRunner is the minimal interface the Watcher needs from ingest.Pipeline.
// ingest.Pipeline already satisfies it. The interface exists so tests can
// inject a fake runner without wiring up a full Pipeline with exiftool/git/pmset.
type IngestRunner interface {
	RunIngest() (ingest.Counts, error)
}

// diskutilCandidates is the ordered list of known diskutil locations on macOS.
// Package-level var (not const) so FindDiskutil tests can force the LookPath branch.
var diskutilCandidates = []string{
	"/usr/sbin/diskutil",
	"/usr/bin/diskutil",
}

// FindDiskutil returns the absolute path to the diskutil binary. Checks known
// macOS locations first, then falls back to exec.LookPath. Returns an
// actionable error if neither finds it.
func FindDiskutil() (string, error) {
	for _, p := range diskutilCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("diskutil"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("diskutil not found (expected on macOS at /usr/sbin/diskutil)")
}

// IsRemovableMedia runs `<diskutilPath> info <volPath>` and reports whether
// any output line matches "Removable Media:.*Removable" — the same grep
// pattern the original on_sd_mount.sh used. Taking the resolved binary as a
// parameter (not calling FindDiskutil internally) makes this testable with a
// fake script, mirroring ReadExif and IsOnACPower.
func IsRemovableMedia(diskutilPath, volPath string) (bool, error) {
	out, err := exec.Command(diskutilPath, "info", volPath).Output()
	if err != nil {
		return false, fmt.Errorf("diskutil info %s: %w", volPath, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, "Removable Media:")
		if idx < 0 {
			continue
		}
		// Check only the value part (after "Removable Media:") so that the word
		// "Removable" in the key name does not cause a false positive when the
		// value is "Fixed" or "Not Removable".
		value := line[idx+len("Removable Media:"):]
		if strings.Contains(value, "Removable") && !strings.Contains(value, "Not Removable") {
			return true, nil
		}
	}
	return false, nil
}

// HasDCIM reports whether volPath contains a DCIM subdirectory. Not injected
// because it is just a directory check — no external binary involved.
func HasDCIM(volPath string) bool {
	fi, err := os.Stat(filepath.Join(volPath, "DCIM"))
	return err == nil && fi.IsDir()
}

// FindSDCard lists the immediate subdirectories of volumesRoot, and returns
// the first one (in name order) that is both removable media and contains a
// DCIM directory. Returns "", nil when no match is found — "no SD card
// present" is a normal outcome, not a failure.
func FindSDCard(diskutilPath, volumesRoot string) (string, error) {
	entries, err := os.ReadDir(volumesRoot)
	if err != nil {
		return "", fmt.Errorf("sdcard: readdir %s: %w", volumesRoot, err)
	}
	// os.ReadDir already returns entries sorted by name, matching the bash
	// loop's implicit alphabetical order over glob results.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volPath := filepath.Join(volumesRoot, e.Name())
		removable, err := IsRemovableMedia(diskutilPath, volPath)
		if err != nil {
			continue // diskutil error for one volume shouldn't stop the scan
		}
		if removable && HasDCIM(volPath) {
			return volPath, nil
		}
	}
	return "", nil
}

// CopyDCIM recursively copies the contents of sdDCIMPath into inboxPath,
// preserving relative directory structure (DCIM/100CANON/IMG_0001.JPG →
// inbox/100CANON/IMG_0001.JPG). This matches `cp -rn DCIM/* inbox/`:
// non-clobbering — if a file already exists at the destination it is skipped
// entirely, protecting a file left over from a previous interrupted run.
// Returns the count of files actually copied (not counting skips).
func CopyDCIM(sdDCIMPath, inboxPath string) (int, error) {
	var count int
	err := filepath.WalkDir(sdDCIMPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sdDCIMPath, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(inboxPath, rel)

		// Non-clobbering: skip if destination already exists.
		if _, err := os.Stat(dst); err == nil {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, dst); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// Watcher watches VolumesRoot for new mounts, detects SD cards among them, and
// calls Runner.RunIngest after copying DCIM contents to InboxPath.
type Watcher struct {
	DiskutilPath string
	VolumesRoot  string
	InboxPath    string
	Runner       IngestRunner
	Logger       *logging.Logger

	// processed tracks volume names already handled during the current mount
	// session. Cleared (via delete) when a Remove event fires for that name,
	// so re-inserting the same card is correctly treated as new.
	processed map[string]bool

	mu sync.Mutex
	fw *fsnotify.Watcher // non-nil while Run() is executing
}

// Stop closes the underlying fsnotify watcher, causing Run() to return nil.
// Safe to call from another goroutine; no-op if Run() has not been called.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fw != nil {
		w.fw.Close()
	}
}

// Run watches VolumesRoot for mount/unmount events and fires ingest on SD card
// detection. Blocks until Stop() is called or the underlying watcher fails.
//
// Watch is non-recursive: only entries appearing/disappearing directly under
// VolumesRoot (i.e. actual mount points) generate events. File activity inside
// an already-mounted volume does not.
//
// On each new-entry Create event:
//  1. Skip if already in w.processed (duplicate event guard).
//  2. Sleep 2 s — let a freshly-mounted volume settle before querying it.
//  3. Check IsRemovableMedia && HasDCIM on the specific new path.
//  4. If matched: CopyDCIM → RunIngest. ErrIngestAlreadyRunning is logged
//     and treated as a normal outcome, not a fatal watcher error.
//
// On Remove: evict the name from w.processed so re-insertion triggers anew.
func (w *Watcher) Run() error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("sdcard: fsnotify: %w", err)
	}

	w.mu.Lock()
	w.fw = fw
	if w.processed == nil {
		w.processed = make(map[string]bool)
	}
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.fw = nil
		w.mu.Unlock()
		fw.Close()
	}()

	if err := fw.Add(w.VolumesRoot); err != nil {
		return fmt.Errorf("sdcard: watch %s: %w", w.VolumesRoot, err)
	}

	for {
		select {
		case event, ok := <-fw.Events:
			if !ok {
				return nil // watcher closed via Stop()
			}
			name := filepath.Base(event.Name)
			volPath := event.Name

			switch {
			case event.Op&fsnotify.Create != 0:
				if w.processed[name] {
					continue
				}
				// Settle delay: freshly-mounted volumes may not be queryable immediately.
				time.Sleep(2 * time.Second)

				removable, err := IsRemovableMedia(w.DiskutilPath, volPath)
				if err != nil {
					w.Logger.Log(logging.PrefixCore,
						fmt.Sprintf("diskutil error for %s: %v", name, err))
					continue
				}
				if !removable || !HasDCIM(volPath) {
					continue
				}

				w.processed[name] = true
				w.Logger.Log(logging.PrefixCore, "SD card detected: "+volPath)

				n, err := CopyDCIM(filepath.Join(volPath, "DCIM"), w.InboxPath)
				if err != nil {
					w.Logger.Log(logging.PrefixCore,
						fmt.Sprintf("DCIM copy error: %v", err))
					// Fall through — files copied before the error still need ingest.
				}
				w.Logger.Log(logging.PrefixCore,
					fmt.Sprintf("copied %d files from DCIM", n))

				counts, err := w.Runner.RunIngest()
				if err != nil {
					if errors.Is(err, ingest.ErrIngestAlreadyRunning) {
						w.Logger.Log(logging.PrefixCore,
							"ingest already running, skipping SD card trigger")
					} else {
						w.Logger.Log(logging.PrefixCore,
							fmt.Sprintf("ingest error: %v", err))
					}
				} else {
					w.Logger.Log(logging.PrefixCore,
						fmt.Sprintf("ingest done: %d imported, %d skipped, %d failed",
							counts.Imported, counts.Skipped, counts.Failed))
				}

			case event.Op&fsnotify.Remove != 0:
				delete(w.processed, name)
			}

		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("fsnotify error: %v", err))
		}
	}
}

// copyFile copies src to dst with a sync before returning.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

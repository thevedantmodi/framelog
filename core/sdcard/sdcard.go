// Package sdcard detects SD cards mounted under /Volumes, copies their DCIM
// contents into inbox/ via rclone, and fires RunIngest. It ports the
// detection logic from on_sd_mount.sh — the dual check (diskutil says
// removable AND a DCIM directory is present) naturally excludes other things
// that mount under /Volumes (backup drives, network shares) without any
// explicit special-casing.
//
// The injectable-binary-path pattern from core/exif and core/gitops is
// applied here for both diskutil and rclone: FindDiskutil/backup.FindRclone
// return paths, IsRemovableMedia/CopyDCIM take them as parameters. Tests can
// therefore pass a fake shell script with no real diskutil or rclone present
// — which matters because diskutil only exists on macOS and rclone may not
// be installed. rclone is a hard dependency for the SD card watcher (like
// diskutil): main.go only constructs a Watcher when both are found. CopyDCIM
// delegates the actual rclone invocation and JSON-log progress parsing to
// core/rclonerun, shared with core/backup's Sync.
package sdcard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thevedantmodi/framelog/core/binpath"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/rclonerun"
)

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
	return binpath.Find(diskutilCandidates, "diskutil",
		errors.New("diskutil not found (expected on macOS at /usr/sbin/diskutil)"))
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

// CopyDCIM recursively copies the contents of sdDCIMPath into inboxPath via
// `rclone copy`, preserving relative directory structure
// (DCIM/100CANON/IMG_0001.JPG → inbox/100CANON/IMG_0001.JPG).
// --ignore-existing matches the old `cp -rn` semantics: non-clobbering — if a
// file already exists at the destination it is skipped entirely, protecting a
// file left over from a previous interrupted run. --include restricts the
// copy to config.SupportedExtensions; --ignore-case makes that match
// regardless of the card's filename casing (IMG_0001.JPG vs .jpg).
// Returns the count of files actually copied (not counting skips).
//
// The optional onCopy callback is invoked after each successful file copy with
// the source file's base name and the running copied-so-far count, parsed
// from rclone's JSON log stream by rclonerun.Copy. Pass nil (or omit) when
// progress reporting is not needed.
func CopyDCIM(rclonePath, sdDCIMPath, inboxPath string, onCopy ...func(filename string, n int)) (int, error) {
	var cb func(string, int)
	if len(onCopy) > 0 {
		cb = onCopy[0]
	}

	args := []string{"copy", sdDCIMPath, inboxPath, "--ignore-existing", "--ignore-case"}
	for _, ext := range sortedSupportedExtensions() {
		args = append(args, "--include", "*"+ext)
	}

	return rclonerun.Copy(rclonePath, args, cb)
}

// sortedSupportedExtensions returns config.SupportedExtensions keys sorted,
// so the rclone --include argument list is deterministic (map iteration
// order is not, and non-deterministic args make failures hard to reproduce).
func sortedSupportedExtensions() []string {
	exts := make([]string, 0, len(config.SupportedExtensions))
	for ext := range config.SupportedExtensions {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}

// Watcher polls VolumesRoot for new mounts, detects SD cards, and fires
// RunIngest after copying DCIM contents to InboxPath.
//
// macOS volume mounts are kernel-level synthetic filesystem ops not delivered
// by FSEvents or kqueue; polling /Volumes every PollInterval is the reliable
// fix. Cost is negligible — os.ReadDir on a directory with ~5 entries.
type Watcher struct {
	DiskutilPath string
	RclonePath   string
	VolumesRoot  string
	InboxPath    string
	PollInterval time.Duration
	Runner       ingest.Runner
	Logger       *logging.Logger

	// seen is the set of volume names present on the last tick.
	// processed is the set already handled this mount session.
	// A name leaving seen is evicted from processed so re-insertion fires again.
	seen      map[string]bool
	processed map[string]bool
	mu        sync.Mutex
}

// Run polls VolumesRoot every PollInterval (default 2s) until stop is closed.
func (w *Watcher) Run(stop <-chan struct{}) error {
	interval := w.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}

	w.mu.Lock()
	w.seen = make(map[string]bool)
	w.processed = make(map[string]bool)
	w.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *Watcher) tick() {
	entries, err := os.ReadDir(w.VolumesRoot)
	if err != nil {
		w.Logger.Log(logging.PrefixCore,
			fmt.Sprintf("sdcard: readdir %s: %v", w.VolumesRoot, err))
		return
	}

	current := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			current[e.Name()] = true
		}
	}

	w.mu.Lock()
	seen := w.seen
	processed := w.processed
	w.mu.Unlock()

	// Evict unmounted volumes so re-insertion fires again.
	for name := range seen {
		if !current[name] {
			delete(processed, name)
		}
	}

	// pausedNames collects SD cards seen this tick that were skipped because the
	// pipeline is paused. They're excluded from w.seen below so they look "new"
	// again on the next tick and get retried once resumed, rather than
	// requiring a physical unmount/remount.
	var pausedNames []string

	// Check newly-appeared volumes.
	for name := range current {
		if seen[name] || processed[name] {
			continue
		}
		seen[name] = true // mark seen regardless of whether it's an SD card

		volPath := filepath.Join(w.VolumesRoot, name)
		removable, err := IsRemovableMedia(w.DiskutilPath, volPath)
		if err != nil {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("diskutil error for %s: %v", name, err))
			continue
		}
		if !removable || !HasDCIM(volPath) {
			continue
		}
		if w.Runner.Paused() {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("ingest paused, SD card %s left for retry on resume", volPath))
			pausedNames = append(pausedNames, name)
			continue
		}

		processed[name] = true
		w.Logger.Log(logging.PrefixCore, "SD card detected: "+volPath)
		w.Logger.Log(logging.PrefixCore, "scanning DCIM (may take a moment on slow card readers)...")

		logProgress := func(filename string, n int) {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("copying [%d] %s → inbox/", n, filename))
		}
		n, copyErr := CopyDCIM(w.RclonePath, filepath.Join(volPath, "DCIM"), w.InboxPath, logProgress)
		if copyErr != nil {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("DCIM copy error: %v", copyErr))
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
	}

	for _, name := range pausedNames {
		delete(current, name)
	}

	w.mu.Lock()
	w.seen = current
	w.mu.Unlock()
}

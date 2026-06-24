// Package xmpwatcher watches originals/ for XMP sidecar and image changes made
// by Lightroom, debounces bursts into single commits, and pushes when on AC
// power and Lightroom is closed. It ports the XMPHandler/_run_xmp_watcher
// logic from the Python predecessor while fixing the missing push-gate check
// (Lightroom was not verified to be closed before pushing).
//
// The injectable-binary-path pattern from core/exif and core/gitops is applied
// here for pgrep: FindPgrep returns the path, IsLightroomRunning takes it as
// a parameter. Tests can pass a fake script with no real pgrep present.
package xmpwatcher

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/gitops"
	"github.com/thevedantmodi/framelog/core/logging"
)

// pgrepCandidates is the ordered list of known pgrep binary locations on macOS.
// Package-level var (not const) so FindPgrep tests can force the LookPath branch.
var pgrepCandidates = []string{
	"/usr/bin/pgrep",
	"/usr/sbin/pgrep",
}

// watchedExts is the closed set of extensions the watcher cares about.
// .xmp: Lightroom XMP sidecar writes.
// .jpg, .jpeg, .heic: formats Lightroom edits in-place.
// .xmp: Lightroom XMP sidecar writes (RAF, CR3, and other proprietary RAW).
// .jpg, .jpeg, .heic: formats Lightroom edits in-place.
// watchedExts is the set of extensions the watcher cares about.
// Includes config.EmbeddedXMPExtensions so Lightroom edits to DNG (and any
// future embedded-XMP formats) are detected; runCommit extracts the embedded
// XMP to a sidecar before committing.
var watchedExts = map[string]bool{
	".xmp":  true,
	".jpg":  true,
	".jpeg": true,
	".heic": true,
	".dng":  true, // see config.EmbeddedXMPExtensions
}

// FindPgrep returns the absolute path to the pgrep binary. Checks known macOS
// locations first, then falls back to exec.LookPath. Returns an actionable
// error if neither finds it.
func FindPgrep() (string, error) {
	for _, p := range pgrepCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("pgrep"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("pgrep not found (expected on macOS at /usr/bin/pgrep)")
}

// IsLightroomRunning runs `<pgrepPath> -i lightroom`. Exit code 0 means pgrep
// found a match (Lightroom is running); exit code 1 is pgrep's documented "no
// process matched" and is not an error. Any other exit code or exec failure is
// a genuine error. Taking the resolved binary as a parameter makes this
// testable with a fake script, mirroring IsOnACPower.
func IsLightroomRunning(pgrepPath string) (bool, error) {
	err := exec.Command(pgrepPath, "-i", "lightroom").Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil // normal: no process matched
	}
	return false, fmt.Errorf("pgrep: %w", err)
}

// Watcher watches OriginalsPath recursively for XMP and image changes, debounces
// bursts into a single commit, and pushes when on AC power and Lightroom is closed.
type Watcher struct {
	GitPath          string
	ExiftoolPath     string // used to extract XMP from DNG before committing
	OriginalsPath    string
	PmsetPath        string
	PgrepPath        string
	DB               *sql.DB
	Logger           *logging.Logger
	// DebounceDuration controls how long the watcher waits after the last event
	// before committing. Production code sets this from config.DebounceSeconds.
	// Tests override to e.g. 50ms to run without real delays.
	DebounceDuration time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	pending map[string]bool // distinct changed file paths since last commit
	fw      *fsnotify.Watcher

	// onRunCommit is called at the start of runCommit for test observation only.
	// Set before calling Run(); the goroutine-start happens-before guarantees
	// the write is visible to the AfterFunc callback goroutine without a lock.
	onRunCommit func()
}

// Stop closes the underlying fsnotify watcher, causing Run to return nil.
// Safe to call from another goroutine; no-op before Run is called.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fw != nil {
		w.fw.Close()
	}
}

// Run begins watching OriginalsPath recursively and blocks until Stop is called
// or an unrecoverable error occurs. It walks OriginalsPath on startup to add
// existing subdirectories, skipping any tree rooted at a directory named ".git"
// — watching git's internal churn is both pointless (no watched extensions live
// there) and leaks watches on every commit this watcher itself makes.
func (w *Watcher) Run() error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("xmpwatcher: fsnotify: %w", err)
	}

	w.mu.Lock()
	w.fw = fw
	if w.pending == nil {
		w.pending = make(map[string]bool)
	}
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.fw = nil
		w.mu.Unlock()
		fw.Close()
	}()

	w.Logger.Log(logging.PrefixXMP, "watching "+w.OriginalsPath)

	// Initial walk: add every directory except .git trees.
	err = filepath.WalkDir(w.OriginalsPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return fw.Add(path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("xmpwatcher: walk %s: %w", w.OriginalsPath, err)
	}

	for {
		select {
		case event, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(fw, event)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.Logger.Log(logging.PrefixXMP,
				fmt.Sprintf("fsnotify error: %v", err))
		}
	}
}

// handleEvent processes one fsnotify event.
func (w *Watcher) handleEvent(fw *fsnotify.Watcher, event fsnotify.Event) {
	path := event.Name

	if event.Op&fsnotify.Create != 0 {
		fi, err := os.Stat(path)
		if err == nil && fi.IsDir() {
			// Walk the new tree: MkdirAll can create 2025/12/27/ before fsnotify
			// delivers the CREATE for 2025/, so we'd miss the deeper dirs if we
			// only added path itself.
			filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
				if err != nil || !d.IsDir() {
					return nil
				}
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				fw.Add(p) //nolint:errcheck
				return nil
			})
			return
		}
	}

	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
		ext := strings.ToLower(filepath.Ext(path))
		if watchedExts[ext] {
			w.mu.Lock()
			w.pending[path] = true
			w.mu.Unlock()
			w.scheduleCommit()
		}
	}
}

// scheduleCommit restarts the debounce timer. Each call resets the window so
// rapid bursts (e.g. a Lightroom preset applied to 50 photos) collapse into
// one commit.
func (w *Watcher) scheduleCommit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.DebounceDuration, w.runCommit)
}

// runCommit snapshots and clears pending, updates DB status for any file whose
// name embeds a hash8 prefix, commits unconditionally, then pushes if on AC
// power and Lightroom is closed.
func (w *Watcher) runCommit() {
	if hook := w.onRunCommit; hook != nil {
		hook()
	}

	w.mu.Lock()
	snapshot := w.pending
	w.pending = make(map[string]bool)
	w.timer = nil
	w.mu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	// Update status for every changed file that carries an embedded hash prefix.
	for path := range snapshot {
		hash8, ok := db.ExtractHashPrefix(filepath.Base(path))
		if !ok {
			continue
		}
		if _, err := db.UpdateStatusByHashPrefix(w.DB, hash8, db.StatusEdited); err != nil {
			w.Logger.Log(logging.PrefixXMP,
				fmt.Sprintf("db update error for %s: %v", filepath.Base(path), err))
		}
	}

	// For DNG files, extract embedded XMP to a sidecar so git only tracks the
	// small XMP file, not the full DNG binary. Uses exiftool -xmp -b to dump
	// the raw XMP packet; writes it alongside the DNG with a .xmp extension.
	if w.ExiftoolPath != "" {
		for path := range snapshot {
			if !config.EmbeddedXMPExtensions[strings.ToLower(filepath.Ext(path))] {
				continue
			}
			xmpPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".xmp"
			out, err := exec.Command(w.ExiftoolPath, "-xmp", "-b", path).Output()
			if err != nil {
				w.Logger.Log(logging.PrefixXMP,
					fmt.Sprintf("extract XMP from %s: %v", filepath.Base(path), err))
				continue
			}
			if len(out) == 0 {
				w.Logger.Log(logging.PrefixXMP,
					fmt.Sprintf("extract XMP from %s: empty output — no XMP embedded yet", filepath.Base(path)))
				continue
			}
			w.Logger.Log(logging.PrefixXMP,
				fmt.Sprintf("extracted %d bytes XMP from %s", len(out), filepath.Base(path)))
			if err := os.WriteFile(xmpPath, out, 0o644); err != nil {
				w.Logger.Log(logging.PrefixXMP,
					fmt.Sprintf("write XMP sidecar for %s: %v", filepath.Base(path), err))
			}
		}
	}

	// Commit is unconditional: it is local, cheap, and idempotent.
	msg := fmt.Sprintf("edit: %s (%d files)",
		time.Now().Format("2006-01-02 15:04:05"), len(snapshot))
	committed, err := gitops.Commit(w.GitPath, w.OriginalsPath, msg)
	if err != nil {
		w.Logger.Log(logging.PrefixXMP, fmt.Sprintf("commit error: %v", err))
		return
	}
	if !committed {
		w.Logger.Log(logging.PrefixXMP, "nothing to commit")
		return
	}
	w.Logger.Log(logging.PrefixXMP, "committed: "+msg)

	// Push gate: requires both AC power AND Lightroom closed.
	// The AC-only check ingest uses is not strict enough here — pushing while
	// Lightroom has the volume open is the specific failure this gate prevents.
	onAC, err := gitops.IsOnACPower(w.PmsetPath)
	if err != nil {
		w.Logger.Log(logging.PrefixXMP, fmt.Sprintf("pmset error: %v", err))
		return
	}
	if !onAC {
		w.Logger.Log(logging.PrefixXMP, "push skipped: not on AC power")
		return
	}

	lrRunning, err := IsLightroomRunning(w.PgrepPath)
	if err != nil {
		w.Logger.Log(logging.PrefixXMP, fmt.Sprintf("pgrep error: %v", err))
		return
	}
	if lrRunning {
		w.Logger.Log(logging.PrefixXMP, "push skipped: Lightroom is running")
		return
	}

	pushed, err := gitops.Push(w.GitPath, w.OriginalsPath, true)
	if err != nil {
		w.Logger.Log(logging.PrefixXMP, fmt.Sprintf("push error: %v", err))
		return
	}
	if pushed {
		w.Logger.Log(logging.PrefixXMP, "pushed to remote")
	}
}

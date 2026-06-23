// Package outgestwatcher watches processed/ for new Lightroom exports and
// triggers RunOutgest after a debounce window. It is the deliberate inverse of
// xmpwatcher in one important respect: it watches only the top-level
// ProcessedPath directory and never adds subdirectories to its watch list.
// That single-directory-only design preserves RunOutgest's non-recursive
// invariant — a YYYY/MM folder created by a prior outgest run appearing as a
// Create event is explicitly ignored rather than watched, preventing already-
// organized files from being re-scanned.
package outgestwatcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// OutgestRunner is the minimal interface the Watcher needs from outgest.Pipeline.
// outgest.Pipeline already satisfies it. The interface exists so tests can
// inject a fake runner that just records call counts, decoupling watcher-
// mechanics tests from needing a fully-wired Pipeline with real exiftool.
type OutgestRunner interface {
	RunOutgest() (outgest.Counts, error)
}

// Watcher watches ProcessedPath for new Lightroom exports and debounces bursts
// into a single RunOutgest call.
type Watcher struct {
	ProcessedPath string
	Outgest       OutgestRunner
	Logger        *logging.Logger
	// DebounceDuration controls how long to wait after the last event before
	// calling RunOutgest. Production code sets this from config.DebounceSeconds.
	// Tests override to e.g. 50ms to avoid real delays.
	DebounceDuration time.Duration

	mu    sync.Mutex
	timer *time.Timer
	fw    *fsnotify.Watcher
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

// Run watches ProcessedPath and blocks until Stop is called or an error occurs.
// Only ProcessedPath itself is watched — no subdirectories are ever added,
// neither during startup nor in response to later Create events. This is the
// key difference from xmpwatcher: directory-create events (YYYY/MM folders
// created by outgest or Lightroom) are silently ignored, not recursed into.
func (w *Watcher) Run() error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("outgestwatcher: fsnotify: %w", err)
	}

	w.mu.Lock()
	w.fw = fw
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.fw = nil
		w.mu.Unlock()
		fw.Close()
	}()

	if err := fw.Add(w.ProcessedPath); err != nil {
		return fmt.Errorf("outgestwatcher: watch %s: %w", w.ProcessedPath, err)
	}

	for {
		select {
		case event, ok := <-fw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(event)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.Logger.Log(logging.PrefixOutgest,
				fmt.Sprintf("fsnotify error: %v", err))
		}
	}
}

// handleEvent processes one fsnotify event. Directories are ignored entirely —
// no Add(), no debounce trigger. Only Create/Write events on files with
// supported extensions reach scheduleRun.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	path := event.Name

	// A YYYY/MM directory created by outgest or Lightroom fires a Create event
	// here. Explicitly skip it — do not add it to the watcher, and do not treat
	// its creation as a signal to run outgest (it doesn't contain new exports).
	fi, err := os.Stat(path)
	if err == nil && fi.IsDir() {
		return
	}

	if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		ext := strings.ToLower(filepath.Ext(path))
		if config.SupportedExtensions[ext] {
			w.scheduleRun()
		}
	}
}

// scheduleRun restarts the debounce timer. Unlike xmpwatcher there is no need
// to accumulate which specific files changed — RunOutgest's own top-level
// ReadDir already covers every eligible file in ProcessedPath on each call.
func (w *Watcher) scheduleRun() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.DebounceDuration, w.runOnce)
}

// runOnce calls RunOutgest once. ErrOutgestAlreadyRunning is an expected
// outcome under load and is logged without treating it as a watcher failure.
// Any other error is logged with its text. On success, no extra line is emitted
// here — RunOutgest's own "Done: N moved..." summary already appeared in the log.
func (w *Watcher) runOnce() {
	_, err := w.Outgest.RunOutgest()
	if err == nil {
		return
	}
	if errors.Is(err, outgest.ErrOutgestAlreadyRunning) {
		w.Logger.Log(logging.PrefixOutgest, "skipped: outgest already running")
		return
	}
	w.Logger.Log(logging.PrefixOutgest, fmt.Sprintf("RunOutgest error: %v", err))
}

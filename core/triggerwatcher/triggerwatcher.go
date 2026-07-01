// Package triggerwatcher implements v1 IPC polling for both trigger files
// (PROTOCOL.md §2). It polls ~/Photos/.ingest_trigger and
// ~/Photos/.outgest_trigger on a configurable interval and fires the
// corresponding runner when a file is found.
//
// Remove-before-act ordering: each trigger file is os.Remove'd BEFORE calling
// the runner. If the runner errors mid-run, the file is already gone, so the
// same trigger does not fire again on the next tick. This is the same
// invariant sdcard uses for ingest: a failed run is logged and the pipeline
// moves on rather than looping on a stuck trigger forever.
package triggerwatcher

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// Watcher polls for trigger files and fires the corresponding runner.
type Watcher struct {
	IngestTriggerPath  string
	OutgestTriggerPath string
	// PollInterval is 2s in production; tests override to avoid real delays.
	PollInterval time.Duration
	Ingest       ingest.Runner
	Outgest      outgest.Runner
	Logger       *logging.Logger
}

// Run loops on PollInterval until stop is closed. Each tick checks both trigger
// files; if present, removes the file first (before acting), then fires the
// runner. Returns nil when stop is closed.
func (w *Watcher) Run(stop <-chan struct{}) error {
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			w.checkIngest()
			w.checkOutgest()
		}
	}
}

func (w *Watcher) checkIngest() {
	if _, err := os.Stat(w.IngestTriggerPath); err != nil {
		return // not present — normal
	}
	if w.Ingest.Paused() {
		w.Logger.Log(logging.PrefixCore, "ingest paused, trigger left in place")
		return
	}
	if err := os.Remove(w.IngestTriggerPath); err != nil {
		w.Logger.Log(logging.PrefixCore,
			fmt.Sprintf("triggerwatcher: remove %s: %v", w.IngestTriggerPath, err))
	}
	_, err := w.Ingest.RunIngest()
	if err == nil {
		return
	}
	if errors.Is(err, ingest.ErrIngestAlreadyRunning) {
		w.Logger.Log(logging.PrefixCore, "ingest already running, trigger skipped")
		return
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("ingest error: %v", err))
}

func (w *Watcher) checkOutgest() {
	if _, err := os.Stat(w.OutgestTriggerPath); err != nil {
		return
	}
	if w.Outgest.Paused() {
		w.Logger.Log(logging.PrefixCore, "outgest paused, trigger left in place")
		return
	}
	if err := os.Remove(w.OutgestTriggerPath); err != nil {
		w.Logger.Log(logging.PrefixCore,
			fmt.Sprintf("triggerwatcher: remove %s: %v", w.OutgestTriggerPath, err))
	}
	_, err := w.Outgest.RunOutgest()
	if err == nil {
		return
	}
	if errors.Is(err, outgest.ErrOutgestAlreadyRunning) {
		w.Logger.Log(logging.PrefixCore, "outgest already running, trigger skipped")
		return
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("outgest error: %v", err))
}

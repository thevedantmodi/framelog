package triggerwatcher

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// ---- helpers ----------------------------------------------------------------

func openTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(filepath.Join(t.TempDir(), "test.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// fakeIngest counts RunIngest calls and can return a controlled error.
type fakeIngest struct {
	mu     sync.Mutex
	calls  int
	err    error
	paused bool
}

func (f *fakeIngest) RunIngest() (ingest.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return ingest.Counts{}, f.err
}

func (f *fakeIngest) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeIngest) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeOutgest counts RunOutgest calls and can return a controlled error.
type fakeOutgest struct {
	mu     sync.Mutex
	calls  int
	err    error
	paused bool
}

func (f *fakeOutgest) RunOutgest() (outgest.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return outgest.Counts{}, f.err
}

func (f *fakeOutgest) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeOutgest) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newWatcher(t *testing.T, dir string, fi *fakeIngest, fo *fakeOutgest) (*Watcher, chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	w := &Watcher{
		IngestTriggerPath:  filepath.Join(dir, ".ingest_trigger"),
		OutgestTriggerPath: filepath.Join(dir, ".outgest_trigger"),
		PollInterval:       20 * time.Millisecond,
		Ingest:             fi,
		Outgest:            fo,
		Logger:             openTestLogger(t),
	}
	return w, stop
}

// touchFile creates an empty file at path.
func touchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("touchFile %s: %v", path, err)
	}
}

// ---- tests ------------------------------------------------------------------

// TestIngestTrigger_CalledOnceFileRemoved verifies a present trigger file causes
// exactly one RunIngest call and that the file is removed afterward.
func TestIngestTrigger_CalledOnceFileRemoved(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{}
	fo := &fakeOutgest{}
	w, stop := newWatcher(t, dir, fi, fo)

	touchFile(t, w.IngestTriggerPath)

	go w.Run(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)

	if n := fi.callCount(); n != 1 {
		t.Errorf("RunIngest called %d times, want 1", n)
	}
	if _, err := os.Stat(w.IngestTriggerPath); err == nil {
		t.Error("ingest trigger file still exists after run")
	}
}

// TestOutgestTrigger_CalledOnceFileRemoved mirrors the ingest test for outgest.
func TestOutgestTrigger_CalledOnceFileRemoved(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{}
	fo := &fakeOutgest{}
	w, stop := newWatcher(t, dir, fi, fo)

	touchFile(t, w.OutgestTriggerPath)

	go w.Run(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)

	if n := fo.callCount(); n != 1 {
		t.Errorf("RunOutgest called %d times, want 1", n)
	}
	if _, err := os.Stat(w.OutgestTriggerPath); err == nil {
		t.Error("outgest trigger file still exists after run")
	}
}

// TestIngestTrigger_PausedLeavesFileInPlace verifies that a paused ingest
// pipeline neither consumes the trigger file nor calls RunIngest, so the
// trigger fires once the pipeline is resumed instead of being silently lost.
func TestIngestTrigger_PausedLeavesFileInPlace(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{paused: true}
	fo := &fakeOutgest{}
	w, stop := newWatcher(t, dir, fi, fo)

	touchFile(t, w.IngestTriggerPath)

	go w.Run(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)

	if n := fi.callCount(); n != 0 {
		t.Errorf("RunIngest called %d times while paused, want 0", n)
	}
	if _, err := os.Stat(w.IngestTriggerPath); err != nil {
		t.Error("ingest trigger file was removed while paused")
	}
}

// TestOutgestTrigger_PausedLeavesFileInPlace mirrors the ingest test for outgest.
func TestOutgestTrigger_PausedLeavesFileInPlace(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{}
	fo := &fakeOutgest{paused: true}
	w, stop := newWatcher(t, dir, fi, fo)

	touchFile(t, w.OutgestTriggerPath)

	go w.Run(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)

	if n := fo.callCount(); n != 0 {
		t.Errorf("RunOutgest called %d times while paused, want 0", n)
	}
	if _, err := os.Stat(w.OutgestTriggerPath); err != nil {
		t.Error("outgest trigger file was removed while paused")
	}
}

// TestAlreadyRunning_NoLoopNoPanic ensures that an AlreadyRunning error does not
// cause a panic and that a subsequent tick with no new trigger file does NOT re-
// fire the runner.
func TestAlreadyRunning_NoLoopNoPanic(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{err: ingest.ErrIngestAlreadyRunning}
	fo := &fakeOutgest{}
	w, stop := newWatcher(t, dir, fi, fo)

	// Place trigger once; runner returns AlreadyRunning.
	touchFile(t, w.IngestTriggerPath)

	go w.Run(stop)
	// Wait enough for 3+ ticks — only 1 call should have fired (the trigger was
	// removed on the first tick, so subsequent ticks find no file).
	time.Sleep(150 * time.Millisecond)
	close(stop)

	if n := fi.callCount(); n != 1 {
		t.Errorf("RunIngest called %d times after single trigger, want 1", n)
	}
}

// TestBothTriggersInOneTick verifies that when both trigger files are present
// in the same tick, both runners are called.
func TestBothTriggersInOneTick(t *testing.T) {
	dir := t.TempDir()
	fi := &fakeIngest{}
	fo := &fakeOutgest{}
	w, stop := newWatcher(t, dir, fi, fo)

	touchFile(t, w.IngestTriggerPath)
	touchFile(t, w.OutgestTriggerPath)

	go w.Run(stop)
	time.Sleep(100 * time.Millisecond)
	close(stop)

	if n := fi.callCount(); n != 1 {
		t.Errorf("RunIngest called %d times, want 1", n)
	}
	if n := fo.callCount(); n != 1 {
		t.Errorf("RunOutgest called %d times, want 1", n)
	}
}

// TestStop_ReturnsPromptly asserts Run returns within one extra poll interval
// after the stop channel is closed, not after a full extra poll.
func TestStop_ReturnsPromptly(t *testing.T) {
	dir := t.TempDir()
	w, stop := newWatcher(t, dir, &fakeIngest{}, &fakeOutgest{})

	done := make(chan struct{})
	go func() {
		w.Run(stop)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	close(stop)

	select {
	case <-done:
		// passed
	case <-time.After(5 * w.PollInterval):
		t.Error("Run did not return promptly after stop was closed")
	}
}

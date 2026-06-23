package outgestwatcher

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// ---- helpers ----------------------------------------------------------------

func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "catalog.db"), false)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.InitDB(conn); err != nil {
		t.Fatalf("db.InitDB: %v", err)
	}
	return conn
}

func openTestLogger(t *testing.T) (*logging.Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := logging.New(path)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// countingRunner is a fake OutgestRunner that just increments a counter.
type countingRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *countingRunner) RunOutgest() (outgest.Counts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return outgest.Counts{}, nil
}

func (r *countingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newWatcher builds a Watcher with the given runner and debounce, logging to a
// temp file. Returns the watcher and the log file path for assertion.
func newWatcher(t *testing.T, processedPath string, runner OutgestRunner, debounce time.Duration) (*Watcher, string) {
	t.Helper()
	logger, logPath := openTestLogger(t)
	return &Watcher{
		ProcessedPath:    processedPath,
		Outgest:          runner,
		Logger:           logger,
		DebounceDuration: debounce,
	}, logPath
}

// ---- Debounce collapses burst -----------------------------------------------

// TestDebounce_CollapsesBurst fires 4 Create events for supported-extension files
// within a tight window and asserts exactly ONE RunOutgest call results.
func TestDebounce_CollapsesBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify timing")
	}

	processed := t.TempDir()
	runner := &countingRunner{}
	w, _ := newWatcher(t, processed, runner, 100*time.Millisecond)

	go w.Run()
	time.Sleep(200 * time.Millisecond) // let startup settle

	// Write 4 supported-extension files in rapid succession.
	for i := range 4 {
		p := filepath.Join(processed, fmt.Sprintf("export%02d.jpg", i))
		if err := os.WriteFile(p, []byte("jpeg"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Wait for debounce (100ms) + runOnce overhead + margin.
	time.Sleep(600 * time.Millisecond)
	w.Stop()

	if n := runner.count(); n != 1 {
		t.Errorf("RunOutgest called %d times, want exactly 1 (burst collapsed)", n)
	}
}

// ---- Directory-create events are ignored ------------------------------------

// TestDirectoryCreate_Ignored asserts that creating a SUBDIRECTORY under
// ProcessedPath (e.g. the YYYY/MM folder outgest itself creates) never triggers
// RunOutgest. This locks in the "opposite of xmpwatcher" design decision.
func TestDirectoryCreate_Ignored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify timing")
	}

	processed := t.TempDir()
	runner := &countingRunner{}
	w, _ := newWatcher(t, processed, runner, 100*time.Millisecond)

	go w.Run()
	time.Sleep(200 * time.Millisecond)

	// Create a YYYY/MM subdirectory directly under ProcessedPath — this is
	// exactly what RunOutgest does internally; the watcher must not react to it.
	subdir := filepath.Join(processed, "2026", "06")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Wait well past the debounce window.
	time.Sleep(500 * time.Millisecond)
	w.Stop()

	if n := runner.count(); n != 0 {
		t.Errorf("RunOutgest called %d times after directory create, want 0", n)
	}
}

// ---- Unsupported extension is ignored ---------------------------------------

func TestUnsupportedExtension_Ignored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify timing")
	}

	processed := t.TempDir()
	runner := &countingRunner{}
	w, _ := newWatcher(t, processed, runner, 100*time.Millisecond)

	go w.Run()
	time.Sleep(200 * time.Millisecond)

	// .txt is not in config.SupportedExtensions.
	if err := os.WriteFile(filepath.Join(processed, "notes.txt"), []byte("text"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	w.Stop()

	if n := runner.count(); n != 0 {
		t.Errorf("RunOutgest called %d times for unsupported extension, want 0", n)
	}
}

// ---- ErrOutgestAlreadyRunning is handled gracefully -------------------------

// TestAlreadyRunning_Graceful asserts that when the runner always returns
// ErrOutgestAlreadyRunning, the watcher logs the expected message and does not
// crash or leak goroutines.
func TestAlreadyRunning_Graceful(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify timing")
	}

	processed := t.TempDir()

	var called atomic.Int32
	alwaysRunning := OutgestRunnerFunc(func() (outgest.Counts, error) {
		called.Add(1)
		return outgest.Counts{}, outgest.ErrOutgestAlreadyRunning
	})

	w, logPath := newWatcher(t, processed, alwaysRunning, 100*time.Millisecond)
	go w.Run()
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(processed, "export.raf"), []byte("raw"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(600 * time.Millisecond)
	w.Stop()

	if called.Load() == 0 {
		t.Fatal("runner was never called")
	}

	// Log must contain the "already running" message.
	found := false
	for _, line := range readLines(t, logPath) {
		if strings.Contains(line, "already running") {
			found = true
			break
		}
	}
	if !found {
		lines := readLines(t, logPath)
		t.Errorf("no 'already running' line in log; got:\n%s", strings.Join(lines, "\n"))
	}
}

// OutgestRunnerFunc is an adapter so a plain func satisfies OutgestRunner.
type OutgestRunnerFunc func() (outgest.Counts, error)

func (f OutgestRunnerFunc) RunOutgest() (outgest.Counts, error) { return f() }

// ---- Integration: real Pipeline ---------------------------------------------

// TestIntegration_RealPipeline wires a real outgest.Pipeline to the watcher
// and asserts that two export files written to ProcessedPath are actually moved
// into YYYY/MM subdirectories after the debounce fires. This proves the
// watcher-to-Pipeline wiring works end-to-end, not just the debounce mechanics.
func TestIntegration_RealPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test: requires fake exiftool + fsnotify")
	}

	const captureDate = "2026:06:22 14:03:11"
	binDir := t.TempDir()
	processed := t.TempDir()

	exiftool := writeFakeBin(t, binDir, "exiftool",
		fmt.Sprintf(`echo '[{"DateTimeOriginal":"%s"}]'`, captureDate))

	pipelineLogger, _ := openTestLogger(t)
	pipeline := &outgest.Pipeline{
		DB:            openTestDB(t),
		Logger:        pipelineLogger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	watcherLogger, _ := openTestLogger(t)
	w := &Watcher{
		ProcessedPath:    processed,
		Outgest:          pipeline,
		Logger:           watcherLogger,
		DebounceDuration: 100 * time.Millisecond,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- w.Run() }()
	time.Sleep(200 * time.Millisecond)

	// Write two .jpg exports with hash8 segments.
	file1 := filepath.Join(processed, "20260622_140311_aabbccdd.jpg")
	file2 := filepath.Join(processed, "20260622_140311_11223344.jpg")
	for _, p := range []string{file1, file2} {
		if err := os.WriteFile(p, []byte("jpeg export"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	// Wait for debounce + RunOutgest + filesystem moves.
	time.Sleep(800 * time.Millisecond)
	w.Stop()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() returned non-nil after Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run() did not return after Stop()")
	}

	// Both files must now be under processed/2026/06/.
	for _, name := range []string{
		"20260622_140311_aabbccdd.jpg",
		"20260622_140311_11223344.jpg",
	} {
		want := filepath.Join(processed, "2026", "06", name)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("file not found at %s after outgest: %v", want, err)
		}
		// Source must be gone from the processed root.
		src := filepath.Join(processed, name)
		if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("source still present at %s after outgest", src)
		}
	}
}

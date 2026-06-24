// ingest_invariants_test.go documents every cross-cutting guarantee the ingest
// package makes. Each test is named after the invariant it enforces so that a
// failing CI run identifies the broken contract by name without reading test bodies.
package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thevedantmodi/framelog/core/db"
)

// TestInvariant_CopyBeforeDelete verifies the central FL-201 guarantee: when
// any step after hashing but before the DB insert fails, the source file must
// remain in inbox so the next RunIngest can safely retry. The "copy before
// delete" ordering is what makes ingest restartable without data loss.
func TestInvariant_CopyBeforeDelete(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	// Make OriginalsPath point through a file so MkdirAll (step 6) fails.
	// Steps 1-5 succeed; step 6 fails; source must remain in inbox.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.OriginalsPath = filepath.Join(blocker, "originals")

	src := writePhoto(t, inbox, "fail.raf")

	r, err := p.ImportFile(src, "b1")
	if err == nil {
		t.Error("expected an error from failed MkdirAll, got nil")
	}
	if r != ResultFailed {
		t.Errorf("result = %q, want %q", r, ResultFailed)
	}

	// Source must still be in inbox.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source file missing from inbox after failed import: %v", err)
	}

	// No DB row must have been inserted.
	n, err := db.PhotoCount(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("photo_count = %d after failed import, want 0", n)
	}
}

// TestInvariant_ConcurrentRunRejected verifies that RunIngest returns
// ErrIngestAlreadyRunning immediately — without touching inbox or the DB —
// when a concurrent invocation already holds the pipeline lock. This is the
// FL-203 guarantee: the core, not the caller, enforces single-flight ingest.
func TestInvariant_ConcurrentRunRejected(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	// Plant a file so we can confirm it wasn't touched.
	writePhoto(t, inbox, "sentinel.raf")

	// Simulate another goroutine already holding the lock.
	if !p.TryAcquire() {
		t.Fatal("pre-acquire failed unexpectedly")
	}

	_, err := p.RunIngest()
	if err == nil {
		t.Fatal("RunIngest returned nil error, want ErrIngestAlreadyRunning")
	}
	if !errors.Is(err, ErrIngestAlreadyRunning) {
		t.Fatalf("err = %v, want ErrIngestAlreadyRunning", err)
	}

	// Inbox file must be completely untouched — proves short-circuit, not just right error.
	if _, statErr := os.Stat(filepath.Join(inbox, "sentinel.raf")); statErr != nil {
		t.Errorf("sentinel.raf missing from inbox after guarded rejection: %v", statErr)
	}

	n, err := db.PhotoCount(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("photo_count = %d after guarded rejection, want 0", n)
	}

	p.Release()
}

// TestInvariant_BackupIndependentOfGitPush verifies the FL-207 guarantee:
// backup.Sync fires after a successful import even when the machine is on
// battery power (and git push is therefore skipped). Backup protects photo
// bytes; git push protects XMP sidecars — they are deliberately decoupled.
func TestInvariant_BackupIndependentOfGitPush(t *testing.T) {
	// newPipeline uses a pmset script that reports Battery Power,
	// so git push will be skipped. Backup must still fire.
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	binDir := t.TempDir()
	backupPath := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "rclone-args")
	p.RclonePath = fakeRclone(t, binDir, argsFile)
	p.BackupPath = backupPath

	writePhoto(t, inbox, "photo.raf")
	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts.Imported != 1 {
		t.Errorf("Imported = %d, want 1", counts.Imported)
	}

	if _, statErr := os.Stat(argsFile); statErr != nil {
		t.Errorf("rclone was not invoked despite successful import on battery power: %v", statErr)
	}
}

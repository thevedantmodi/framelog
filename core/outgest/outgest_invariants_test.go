// outgest_invariants_test.go documents every cross-cutting guarantee the
// outgest package makes. Each test is named after the invariant it enforces.
package outgest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInvariant_NonRecursiveListing verifies the FL-202 guarantee: RunOutgest
// lists only the top level of ProcessedPath and never re-scans files that are
// already inside a YYYY/MM subdirectory created by a prior outgest run. A
// recursive listing would double-move already-organized files on the next run.
func TestInvariant_NonRecursiveListing(t *testing.T) {
	binDir := t.TempDir()
	processed := t.TempDir()
	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftool(testDate))
	conn := openTestDB(t)
	logger, _ := openTestLogger(t)

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	// Two organizable files at the top level.
	writeFile(t, processed, "20260622_140311_aabbccdd.jpg", "export A")
	writeFile(t, processed, "20260622_140311_11223344.cr3", "export B")
	// Unsupported extension — must be left alone.
	unsupported := writeFile(t, processed, "notes.txt", "ignore me")
	// File already inside a YYYY/MM subdir — must NOT be re-scanned or moved.
	alreadyOrganizedDir := filepath.Join(processed, "2025", "12")
	if err := os.MkdirAll(alreadyOrganizedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alreadyOrganized := writeFile(t, alreadyOrganizedDir, "20251201_120000_deadbeef.jpg", "old export")

	counts, err := p.RunOutgest()
	if err != nil {
		t.Fatalf("RunOutgest: %v", err)
	}

	if counts.Moved != 2 {
		t.Errorf("Moved = %d, want 2", counts.Moved)
	}
	if counts.Failed != 0 {
		t.Errorf("Failed = %d, want 0", counts.Failed)
	}

	// Unsupported file untouched.
	if _, err := os.Stat(unsupported); err != nil {
		t.Errorf("unsupported file was touched: %v", err)
	}

	// File in YYYY/MM subdir must still be exactly where it was — not moved again.
	if _, err := os.Stat(alreadyOrganized); err != nil {
		t.Errorf("already-organized file was moved or deleted: %v", err)
	}

	// Must NOT have been re-organized into a deeper subdir.
	badPath := filepath.Join(alreadyOrganizedDir, "2026", "06", "20251201_120000_deadbeef.jpg")
	if _, err := os.Stat(badPath); err == nil {
		t.Error("already-organized file was incorrectly re-organized into a deeper subdir")
	}
}

// TestInvariant_ConcurrentRunRejected verifies that RunOutgest returns
// ErrOutgestAlreadyRunning immediately — without touching ProcessedPath —
// when a concurrent invocation already holds the pipeline lock. This is the
// FL-205 guarantee: the core, not the caller, enforces single-flight outgest.
func TestInvariant_ConcurrentRunRejected(t *testing.T) {
	p, processed := newPipeline(t, testDate)

	// Plant a file so we can confirm it was not touched.
	writeFile(t, processed, "sentinel_aabbccdd.jpg", "data")

	if !p.TryAcquire() {
		t.Fatal("pre-acquire failed unexpectedly")
	}

	_, err := p.RunOutgest()
	if err == nil {
		t.Fatal("RunOutgest returned nil, want ErrOutgestAlreadyRunning")
	}
	if !errors.Is(err, ErrOutgestAlreadyRunning) {
		t.Fatalf("err = %v, want ErrOutgestAlreadyRunning", err)
	}

	// Sentinel must still be in ProcessedPath root — proves short-circuit.
	if _, statErr := os.Stat(filepath.Join(processed, "sentinel_aabbccdd.jpg")); statErr != nil {
		t.Errorf("sentinel file missing from ProcessedPath after guard rejection: %v", statErr)
	}

	p.Release()
}

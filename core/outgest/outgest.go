// Package outgest organises Lightroom exports from processed/ into YYYY/MM
// subdirectories and marks the matching catalog rows as "published". It mirrors
// the Python predecessor's outgest.py but with one key fix: status is now
// actually set (the old code read the hash prefix from the filename and then
// never used it to update the DB).
package outgest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/exif"
	"github.com/thevedantmodi/framelog/core/logging"
)

// ErrOutgestAlreadyRunning is returned by RunOutgest when another invocation is
// in progress. String value matches the wire error code convention from
// PROTOCOL.md §3 (same pattern as ingest's ErrIngestAlreadyRunning).
var ErrOutgestAlreadyRunning = errors.New("outgest_already_running")

// Runner is the minimal interface consumers of outgest need. *Pipeline satisfies
// it. Defined here so packages that depend on outgest behaviour (outgestwatcher,
// ipc, triggerwatcher) can accept a fake in tests without a full Pipeline.
type Runner interface {
	RunOutgest() (Counts, error)
}

// Result describes the outcome of a single OrganizeFile call.
type Result string

const (
	ResultMoved   Result = "moved"
	ResultSkipped Result = "skipped"
	ResultFailed  Result = "failed"
)

// Counts tallies the per-run outcomes.
type Counts struct{ Moved, Skipped, Failed int }

// Pipeline holds resolved dependencies for an outgest run.
type Pipeline struct {
	DB            *sql.DB
	Logger        *logging.Logger
	ProcessedPath string
	ExiftoolPath  string

	mu      sync.Mutex
	running bool
}

// TryAcquire attempts to mark RunOutgest as running. Returns true when the
// pipeline was idle and the flag is now set; returns false without blocking
// when another call is already in progress. Exported so tests can exercise
// the locking primitive in isolation.
func (p *Pipeline) TryAcquire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return false
	}
	p.running = true
	return true
}

// Release clears the running flag set by TryAcquire. Exported to pair with
// TryAcquire in tests.
func (p *Pipeline) Release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
}

// OutgestRunning reports whether a RunOutgest call is currently in progress.
// Mirrors ingest.Pipeline.IngestRunning — used by the IPC status handler.
func (p *Pipeline) OutgestRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// captureLayout is exiftool's DateTimeOriginal format, identical to what
// ingest.go uses — both packages parse the same exif.Metadata.CaptureDate.
const captureLayout = "2006:01:02 15:04:05"

// OrganizeFile moves path into a YYYY/MM subdirectory derived from its EXIF
// capture date, then marks the matching catalog row as "published" using the
// 8-char hash prefix embedded in the filename.
//
// Steps:
//  1. ReadExif for CaptureDate; parse year/month.
//  2. Build destDir as a sibling YYYY/MM of the file's current location.
//     os.Rename is safe here because source and dest are on the same
//     filesystem — destDir is always under filepath.Dir(path), never a
//     cross-volume move.
//  3. If dest already exists → ResultSkipped (idempotent on repeated runs).
//  4. MkdirAll + Rename.
//  5. Extract hash8 from filename; if found, UpdateStatusByHashPrefix →
//     StatusPublished. A false return (no DB row) is silently ignored —
//     the file may predate the catalog.
//  6. Return ResultMoved.
func (p *Pipeline) OrganizeFile(path string) (Result, error) {
	// Step 1: capture date for the YYYY/MM destination.
	meta, err := exif.ReadExif(path, p.ExiftoolPath)
	if err != nil {
		return p.fail(path, fmt.Errorf("exif: %w", err))
	}
	captureTime, err := time.ParseInLocation(captureLayout, meta.CaptureDate, time.Local)
	if err != nil {
		return p.fail(path, fmt.Errorf("parse capture date %q: %w", meta.CaptureDate, err))
	}

	// Step 2: destDir is a sibling of the source file's directory — same
	// filesystem as source, so os.Rename never crosses a volume boundary.
	destDir := filepath.Join(
		filepath.Dir(path),
		fmt.Sprintf("%04d", captureTime.Year()),
		fmt.Sprintf("%02d", int(captureTime.Month())),
	)
	dest := filepath.Join(destDir, filepath.Base(path))

	// Step 3: already organized on a previous run → idempotent skip.
	if _, err := os.Stat(dest); err == nil {
		return ResultSkipped, nil
	}

	// Step 4: create the subdirectory and move the file.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return p.fail(path, fmt.Errorf("mkdir %s: %w", destDir, err))
	}
	if err := os.Rename(path, dest); err != nil {
		return p.fail(path, fmt.Errorf("rename to %s: %w", dest, err))
	}

	// Step 5: update catalog status if the filename contains a hash8 segment.
	if hash8, ok := db.ExtractHashPrefix(filepath.Base(dest)); ok {
		_, err := db.UpdateStatusByHashPrefix(p.DB, hash8, db.StatusPublished)
		if err != nil {
			// A real DB error (not "no row matched") — log and fail so the
			// caller knows the move succeeded but the status update didn't.
			return p.fail(dest, fmt.Errorf("update status: %w", err))
		}
		// false return (no row matched) is silently ignored per spec.
	}

	return ResultMoved, nil
}

// fail logs the failure and returns ResultFailed.
func (p *Pipeline) fail(path string, err error) (Result, error) {
	p.Logger.Log(logging.PrefixOutgest,
		fmt.Sprintf("FAILED %s: %v", filepath.Base(path), err))
	return ResultFailed, err
}

// RunOutgest reads the top level of ProcessedPath (non-recursive — files
// already organized into YYYY/MM subdirs must not be re-scanned), filters to
// supported extensions, and calls OrganizeFile on each. Returns
// ErrOutgestAlreadyRunning immediately if another call is in progress.
func (p *Pipeline) RunOutgest() (Counts, error) {
	if !p.TryAcquire() {
		return Counts{}, ErrOutgestAlreadyRunning
	}
	defer p.Release()

	entries, err := os.ReadDir(p.ProcessedPath)
	if err != nil {
		return Counts{}, fmt.Errorf("outgest: readdir %s: %w", p.ProcessedPath, err)
	}

	// Collect plain files with supported extensions, sorted for deterministic
	// order matching Python's sorted(processed.iterdir()).
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue // YYYY/MM subdirs — already organized, skip.
		}
		if config.SupportedExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			files = append(files, filepath.Join(p.ProcessedPath, e.Name()))
		}
	}
	sort.Strings(files)

	var counts Counts
	for _, f := range files {
		switch result, _ := p.OrganizeFile(f); result {
		case ResultMoved:
			counts.Moved++
		case ResultSkipped:
			counts.Skipped++
		case ResultFailed:
			counts.Failed++
		}
	}

	p.Logger.Log(logging.PrefixOutgest,
		fmt.Sprintf("Done: %d moved, %d skipped, %d failed",
			counts.Moved, counts.Skipped, counts.Failed))

	return counts, nil
}

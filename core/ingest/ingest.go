// Package ingest copies photos from inbox/ to originals/, writes XMP sidecars,
// records them in the catalog DB, and commits the originals git repo after a
// successful batch. This is the first orchestration layer — it wires together
// every Phase 1 package (hasher, exif, xmp, db, gitops, logging) without
// duplicating any logic from them.
//
// The invariant this package is built around is "copy before delete": the
// source file in inbox/ is only removed after the DB insert succeeds. If any
// earlier step fails, the source stays in inbox so the next RunIngest call can
// safely retry it. Don't reorder the steps in ImportFile without thinking
// carefully about what that retry story looks like.
package ingest

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/exif"
	"github.com/thevedantmodi/framelog/core/gitops"
	"github.com/thevedantmodi/framelog/core/hasher"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/xmp"
)

// ErrIngestAlreadyRunning is returned by RunIngest when another invocation is
// in progress. The string value matches the wire error code in PROTOCOL.md §3
// exactly so the socket handler (FL-302) can use err.Error() as the JSON
// "error" field with no mapping table.
var ErrIngestAlreadyRunning = errors.New("ingest_already_running")

// ErrIngestPaused is returned by RunIngest when the pipeline has been paused
// via Pause(). String value matches the wire error code convention (same
// pattern as ErrIngestAlreadyRunning) so the socket handler can use
// err.Error() directly as the JSON "error" field.
var ErrIngestPaused = errors.New("ingest_paused")

// Runner is the minimal interface consumers of ingest need. *Pipeline satisfies
// it. Defined here so packages that depend on ingest behaviour (sdcard, ipc,
// triggerwatcher) can accept a fake implementation in tests without wiring up a
// full Pipeline with real exiftool/git/pmset. Paused lets callers that trigger
// ingest automatically (sdcard, triggerwatcher) check state before consuming a
// trigger file or copying DCIM contents, rather than losing the signal to a
// paused RunIngest call.
type Runner interface {
	RunIngest() (Counts, error)
	Paused() bool
}

// Pipeline holds resolved dependencies for an ingest run. Binary paths
// (ExiftoolPath, GitPath, PmsetPath) are resolved once by the caller via the
// Find* functions in each package and reused across every file in the batch —
// FindExiftool/FindGit/FindPmset are never called per-file inside this package.
type Pipeline struct {
	DB            *sql.DB
	Logger        *logging.Logger
	InboxPath     string
	OriginalsPath string
	ExiftoolPath  string
	GitPath       string
	PmsetPath     string
	RclonePath    string
	BackupPath    string // initial value; SetBackupPath overrides at runtime

	// OnFileWritten, when non-nil, is called with the destination path of every
	// file this pipeline writes into OriginalsPath (the copied photo and its XMP
	// sidecar). main wires it to xmpwatcher.Watcher.Suppress so ingest's own
	// writes don't trigger the watcher and flip fresh imports to status
	// "edited" (PROTOCOL.md §1: edited means Lightroom wrote to the file).
	OnFileWritten func(path string)

	mu                 sync.Mutex
	running            bool
	paused             atomic.Bool
	backupPathOverride atomic.Pointer[string]
}

// Pause prevents new RunIngest calls from starting. An in-flight run (if any)
// completes normally — Pause only blocks future starts, it does not cancel
// work already underway.
func (p *Pipeline) Pause() { p.paused.Store(true) }

// Resume clears the paused flag set by Pause.
func (p *Pipeline) Resume() { p.paused.Store(false) }

// Paused reports whether the pipeline is currently paused. Checked by
// triggerwatcher and sdcard before consuming a trigger file or DCIM copy, so
// automatic triggers don't silently discard work while paused.
func (p *Pipeline) Paused() bool { return p.paused.Load() }

// SetBackupPath updates the backup path at runtime (thread-safe). Overrides
// the BackupPath field set at construction for all subsequent RunIngest calls.
func (p *Pipeline) SetBackupPath(path string) {
	p.backupPathOverride.Store(&path)
}

// getBackupPath returns the current effective backup path.
func (p *Pipeline) getBackupPath() string {
	if v := p.backupPathOverride.Load(); v != nil {
		return *v
	}
	return p.BackupPath
}

// TryAcquire attempts to mark RunIngest as running. Returns true and sets the
// flag when the pipeline is idle; returns false without blocking when it is
// already running. Exported so tests can exercise the locking primitive in
// isolation without racing against real file I/O timing.
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

// IngestRunning reports whether a RunIngest call is currently in progress.
// Used by the IPC status handler (FL-302) to populate the ingest_running field
// without blocking on any long-running pipeline method.
func (p *Pipeline) IngestRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Result describes the outcome of a single ImportFile call.
type Result string

const (
	ResultImported Result = "imported"
	ResultSkipped  Result = "skipped"
	ResultFailed   Result = "failed"
)

// Counts tallies the per-run outcomes.
type Counts struct{ Imported, Skipped, Failed int }

// captureLayout is exiftool's DateTimeOriginal format. exif.go guarantees
// Metadata.CaptureDate is always in this layout (mtime fallback uses the same).
const captureLayout = "2006:01:02 15:04:05"

// ImportFile imports one file from inbox to originals following the
// copy-before-delete sequence. Steps are numbered to match the spec so
// reviewers can verify ordering at a glance.
func (p *Pipeline) ImportFile(srcPath, batchID string) (Result, error) {
	// 1. Hash.
	hash, err := hasher.HashFile(srcPath)
	if err != nil {
		return p.fail(srcPath, fmt.Errorf("hash: %w", err))
	}

	// 2. Dedup check — skip silently, leave source in inbox.
	exists, err := db.HashExists(p.DB, hash)
	if err != nil {
		return p.fail(srcPath, fmt.Errorf("dedup check: %w", err))
	}
	if exists {
		return ResultSkipped, nil
	}

	// 3. EXIF read using the caller-resolved binary path.
	meta, err := exif.ReadExif(srcPath, p.ExiftoolPath)
	if err != nil {
		return p.fail(srcPath, fmt.Errorf("exif: %w", err))
	}

	// 4. Parse CaptureDate to get calendar components for the dest path.
	captureTime, err := time.ParseInLocation(captureLayout, meta.CaptureDate, time.Local)
	if err != nil {
		return p.fail(srcPath, fmt.Errorf("parse capture date %q: %w", meta.CaptureDate, err))
	}

	// 5. Build dest path: originals/YYYY/MM/DD/YYYYMMDD_HHMMSS_<hash8><ext>.
	ext := strings.ToLower(filepath.Ext(srcPath))
	filename := fmt.Sprintf("%s_%s%s", captureTime.Format("20060102_150405"), hash[:8], ext)
	destDir := filepath.Join(p.OriginalsPath,
		captureTime.Format("2006"),
		captureTime.Format("01"),
		captureTime.Format("02"),
	)
	dest := filepath.Join(destDir, filename)

	// 6. Mkdir.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return p.fail(srcPath, fmt.Errorf("mkdir %s: %w", destDir, err))
	}

	// 7. Copy (not rename/move — source stays in inbox until step 9 succeeds).
	// Suppress the watcher before writing: fsnotify delivers the event moments
	// after the write, so registering afterwards would race.
	if p.OnFileWritten != nil {
		p.OnFileWritten(dest)
	}
	if err := copyFile(srcPath, dest); err != nil {
		return p.fail(srcPath, fmt.Errorf("copy to %s: %w", dest, err))
	}

	// 8. XMP sidecar next to the dest file — skipped for formats that embed XMP
	// internally (see config.EmbeddedXMPExtensions). Writing a sidecar next to
	// those files causes Lightroom to read the sidecar and ignore the embedded
	// data, hiding develop edits.
	if !config.EmbeddedXMPExtensions[ext] {
		if p.OnFileWritten != nil {
			p.OnFileWritten(strings.TrimSuffix(dest, ext) + ".xmp")
		}
		if _, err := xmp.WriteXMP(dest, batchID, meta.CameraModel); err != nil {
			return p.fail(srcPath, fmt.Errorf("xmp: %w", err))
		}
	}

	// 9. DB insert. GPS fields flow straight from Metadata — this is the fix for
	// the Python predecessor's bug where exiftool read coordinates and they were
	// silently dropped before reaching the database (never wired into the insert).
	photo := db.Photo{
		Hash:             hash,
		OriginalFilename: filepath.Base(srcPath),
		ImportedPath:     dest,
		CameraModel:      meta.CameraModel,
		CaptureDate:      meta.CaptureDate,
		ImportTimestamp:  time.Now().UTC().Format(time.RFC3339),
		GPSLat:           meta.GPSLat, // nil when absent, not 0.0
		GPSLon:           meta.GPSLon,
	}
	if err := db.InsertPhoto(p.DB, photo); err != nil {
		// UNIQUE constraint means a previous ingest already recorded this hash
		// (e.g. originals/ was wiped but catalog.db was not). Treat as duplicate.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ResultSkipped, nil
		}
		return p.fail(srcPath, fmt.Errorf("db insert: %w", err))
	}

	// 10. Source removal — only reached when every prior step succeeded.
	if err := os.Remove(srcPath); err != nil {
		// Non-fatal: import is complete; log the cleanup failure and move on.
		p.Logger.Log(logging.PrefixIngest,
			fmt.Sprintf("WARN could not remove source %s: %v", filepath.Base(srcPath), err))
	}

	return ResultImported, nil
}

// fail logs the failure and returns ResultFailed without touching the source.
func (p *Pipeline) fail(srcPath string, err error) (Result, error) {
	p.Logger.Log(logging.PrefixIngest,
		fmt.Sprintf("FAILED %s: %v", filepath.Base(srcPath), err))
	return ResultFailed, err
}

// RunIngest walks InboxPath, imports every supported file, and commits+pushes
// the originals repo when something was imported. Per-file failures are tallied
// in Counts.Failed — they do not cause RunIngest itself to return an error.
// Returns ErrIngestAlreadyRunning immediately if another call is in progress
// (PROTOCOL.md §3: "concurrency is the core's job").
func (p *Pipeline) RunIngest() (Counts, error) {
	if p.Paused() {
		return Counts{}, ErrIngestPaused
	}
	if !p.TryAcquire() {
		return Counts{}, ErrIngestAlreadyRunning
	}
	defer p.Release()

	// batchID[:10] is "YYYY-MM-DD" since RFC3339 begins with the date.
	batchID := time.Now().Format(time.RFC3339)

	if err := db.InitDB(p.DB); err != nil {
		return Counts{}, fmt.Errorf("ingest: InitDB: %w", err)
	}

	// Collect supported files; sort for deterministic order matching Python's
	// sorted(rglob(...)).
	var files []string
	err := filepath.WalkDir(p.InboxPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if config.SupportedExtensions[strings.ToLower(filepath.Ext(path))] {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return Counts{}, fmt.Errorf("ingest: walk inbox: %w", err)
	}
	sort.Strings(files)

	total := len(files)
	var counts Counts
	for i, f := range files {
		p.Logger.Log(logging.PrefixIngest,
			fmt.Sprintf("copying [%d/%d] %s", i+1, total, filepath.Base(f)))
		switch result, _ := p.ImportFile(f, batchID); result {
		case ResultImported:
			counts.Imported++
		case ResultSkipped:
			counts.Skipped++
		case ResultFailed:
			counts.Failed++
		}
	}

	p.Logger.Log(logging.PrefixIngest,
		fmt.Sprintf("Done: %d imported, %d skipped, %d failed",
			counts.Imported, counts.Skipped, counts.Failed))

	msg := fmt.Sprintf("ingest: %s (%d photos)", batchID[:10], counts.Imported)
	committed, err := gitops.Commit(p.GitPath, p.OriginalsPath, msg)
	if err != nil {
		p.Logger.Log(logging.PrefixGit, fmt.Sprintf("commit error: %v", err))
	} else if committed {
		p.Logger.Log(logging.PrefixGit, fmt.Sprintf("commit: %s", msg))
		onAC, err := gitops.IsOnACPower(p.PmsetPath)
		if err != nil {
			p.Logger.Log(logging.PrefixGit, fmt.Sprintf("pmset error: %v", err))
		} else {
			pushed, err := gitops.Push(p.GitPath, p.OriginalsPath, onAC)
			if err != nil {
				p.Logger.Log(logging.PrefixGit, fmt.Sprintf("push error: %v", err))
			} else if pushed {
				p.Logger.Log(logging.PrefixGit, "pushed to remote")
			}
		}
	}

	// Backup: gated only on having imported files and a configured BackupPath.
	// Deliberately independent of the git commit/push block above — git only
	// tracks XMP sidecars; the photo bytes are safe in originals/ as soon as
	// the import loop finishes, regardless of AC power or push outcome.
	backupPath := p.getBackupPath()
	if counts.Imported > 0 && backupPath != "" && p.RclonePath == "" {
		// rclone was not found at startup — a configured backup path cannot be
		// synced. Say so explicitly instead of invoking exec.Command("").
		p.Logger.Log(logging.PrefixBackup, "backup skipped: rclone not installed")
	} else if counts.Imported > 0 && backupPath != "" {
		p.Logger.Log(logging.PrefixBackup, fmt.Sprintf("syncing %d photos to %s", counts.Imported, backupPath))
		synced, err := backup.Sync(p.RclonePath, p.OriginalsPath, backupPath)
		if err != nil {
			p.Logger.Log(logging.PrefixBackup, fmt.Sprintf("sync error: %v", err))
		} else if synced {
			p.Logger.Log(logging.PrefixBackup,
				fmt.Sprintf("synced %d photos to %s", counts.Imported, backupPath))
		} else {
			p.Logger.Log(logging.PrefixBackup, "backup drive not found, skipped")
		}
	}

	return counts, nil
}

// copyFile copies src to dst, syncing the destination before returning so a
// crash immediately after can't leave a partial file that looks complete.
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

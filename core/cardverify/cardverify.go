// Package cardverify answers one question: "can I format this card?"
//
// It re-reads every media file on a mounted camera card, hashes it, and
// proves each file's bytes exist intact in the library (originals/) and,
// when a backup drive is configured, on the backup too. The check is
// end-to-end on purpose: it does not trust rclone's exit code, the inbox
// copy, or the catalog alone. A file only counts as verified when
//
//  1. its SHA-256 on the card is a row in the catalog (it was imported),
//  2. the library file that row points at re-hashes to the same digest, and
//  3. (backup configured) the backup copy re-hashes to the same digest.
//
// Because the catalog is keyed by content hash, this works no matter how a
// file reached the library — this card session, an earlier insert of the same
// card, or a duplicate shot on another card. It also catches every way the
// pipeline can quietly leave something behind: a file rclone skipped, one
// still in inbox/ or quarantined to inbox/failed/, a corrupt copy, a backup
// that never ran, and file types Framelog doesn't import at all (which would
// otherwise be destroyed by a format with nobody noticing).
//
// The result is a Report with a single SafeToFormat verdict plus a plain-text
// receipt in ~/Photos/reports/ and a row in catalog.db (PROTOCOL.md §1).
package cardverify

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
)

// Kind classifies a file found on the card.
type Kind string

const (
	// KindMedia is a file Framelog imports (config.IsImportable).
	// Every media file must be verified before the card is safe to format.
	KindMedia Kind = "media"
	// KindSidecar is metadata that travels with a media file and is not
	// catalogued on its own: the Sony C####M01.XML next to a clip. It is
	// copied by the clip import, so its loss is not a reason to block.
	KindSidecar Kind = "sidecar"
	// KindIgnored is a known camera type the user switched off in the
	// file-type settings. Listed on the report, never blocks: the user
	// chose not to keep these.
	KindIgnored Kind = "ignored"
	// KindUnhandled is anything else in a media directory — a format
	// Framelog doesn't import. Formatting would destroy it, so it blocks.
	KindUnhandled Kind = "unhandled"
)

// CardFile is one file found on the card.
type CardFile struct {
	RelPath string // relative to the volume root, e.g. DCIM/100MSDCF/DSC00001.ARW
	AbsPath string
	Size    int64
	Kind    Kind
}

// File states — the closed set stored in card_report_files.state
// (PROTOCOL.md §1).
const (
	StateVerified        = "verified"
	StateNotInLibrary    = "not_in_library"   // card hash has no catalog row
	StateLibraryMissing  = "library_missing"  // row exists, file gone from originals/
	StateLibraryMismatch = "library_mismatch" // library copy's bytes differ
	StateBackupMissing   = "backup_missing"   // no copy on the backup drive
	StateBackupMismatch  = "backup_mismatch"  // backup copy's bytes differ
	StateCardUnreadable  = "card_unreadable"  // read error on the card itself
	StateUnhandled       = "unhandled"        // file type Framelog never imports
	StateIgnored         = "ignored"          // known type switched off in File Types
)

// Backup states — card_reports.backup_state.
const (
	BackupVerified      = "verified"       // every media file verified on the backup
	BackupNotConfigured = "not_configured" // no backup path set; library-only check
	BackupDriveMissing  = "drive_missing"  // configured but not mounted
	BackupIncomplete    = "incomplete"     // mounted, but some files missing/mismatched
)

// FileResult is the verdict for one card file.
type FileResult struct {
	CardFile
	Hash        string
	LibraryPath string
	BackupPath  string
	State       string
	Detail      string
	// New is true when the library row was created during this card session
	// (import_timestamp >= Options.SessionStart).
	New         bool
	CaptureDate string
	CameraModel string
}

// Report is the outcome of one Verify call.
type Report struct {
	VolumeName string
	VolumeUUID string
	VolumePath string
	BackupPath string
	StartedAt  time.Time
	FinishedAt time.Time

	Files        []FileResult // media + unhandled; sidecars are not listed
	Sidecars     int
	BackupState  string
	SafeToFormat bool
	// Problems are plain sentences explaining why SafeToFormat is false (or,
	// for informational lines, what the user should know). Empty on a clean
	// pass.
	Problems []string
	// Notes are informational lines that do not affect the verdict.
	Notes []string
}

// Options configures Verify.
type Options struct {
	VolumePath    string
	VolumeName    string // defaults to filepath.Base(VolumePath)
	VolumeUUID    string
	DB            *sql.DB
	OriginalsPath string
	// BackupPath is the configured backup root ("" = backup disabled).
	// Copies are expected under backup.DestDir(BackupPath).
	BackupPath string
	// SessionStart marks when this card's copy began; library rows imported
	// at or after it count as "new" in the report. Zero means there was no
	// import session (an on-demand re-verify): nothing counts as new.
	SessionStart time.Time
	// CopiedThisSession, when non-nil, holds the RelPaths this card run
	// copied off the card. A file then counts as new only if it is in the
	// set and was imported since SessionStart — so a re-insert straight
	// after an import (same second) is not miscounted as all-new.
	CopiedThisSession map[string]bool
	// ExtraProblems are failures the caller already knows about (e.g. an
	// rclone error while copying the card) — included verbatim. They are
	// context, not a verdict: the per-file check decides SafeToFormat.
	ExtraProblems []string
	// Progress, if set, is called after each media file is checked. Calls
	// are serialised; the callback need not be goroutine-safe.
	Progress func(done, total int, file string)
	// Workers bounds concurrent library/backup re-hashing. Card reads are
	// always sequential (an SD card is slowest under parallel random reads);
	// destination checks overlap with them. Default 2.
	Workers int
}

// Inventory lists every file under the card's media directories — DCIM/ and
// Sony's PRIVATE/M4ROOT/CLIP/ — classified by Kind, sorted by path. Hidden
// files (macOS "._" AppleDouble files, .DS_Store, .Trashes) are skipped:
// they are created by the Mac, not the camera. Other directories on the card
// (PRIVATE/M4ROOT/THMBNL thumbnails, SUB proxies, camera databases) are
// regenerated by the camera on format and are not inventoried.
func Inventory(volPath string) ([]CardFile, error) {
	var out []CardFile
	roots := []struct {
		rel    string
		isClip bool
	}{
		{config.DCIMDirName, false},
		{config.ClipDirRelPath, true},
	}
	for _, r := range roots {
		root := filepath.Join(volPath, r.rel)
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(volPath, path)
			ext := strings.ToLower(filepath.Ext(path))
			kind := KindUnhandled
			switch {
			case config.IsImportable(ext):
				kind = KindMedia
			case config.IsKnownMediaType(ext):
				kind = KindIgnored
			case r.isClip && ext == config.NRTMetaExtension:
				kind = KindSidecar
			}
			out = append(out, CardFile{RelPath: rel, AbsPath: path, Size: info.Size(), Kind: kind})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("cardverify: walk %s: %w", root, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// Verify inventories the card and checks every media file end to end.
// It returns an error only when the check itself could not run (card
// unreadable as a whole, DB failure); per-file problems are in the Report.
func Verify(opts Options) (*Report, error) {
	if opts.DB == nil {
		return nil, errors.New("cardverify: nil DB")
	}
	start := time.Now()
	sessionStart := opts.SessionStart
	name := opts.VolumeName
	if name == "" {
		name = filepath.Base(opts.VolumePath)
	}

	inv, err := Inventory(opts.VolumePath)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		VolumeName: name,
		VolumeUUID: opts.VolumeUUID,
		VolumePath: opts.VolumePath,
		BackupPath: opts.BackupPath,
		StartedAt:  start,
	}

	// Backup reachability is decided once, up front. A drive that vanishes
	// mid-check shows up per file as backup_missing.
	backupRoot := ""
	switch {
	case opts.BackupPath == "":
		rep.BackupState = BackupNotConfigured
	case !backup.IsDriveMounted(opts.BackupPath):
		rep.BackupState = BackupDriveMissing
	default:
		backupRoot = backup.DestDir(opts.BackupPath)
	}

	var media []CardFile
	for _, f := range inv {
		switch f.Kind {
		case KindMedia:
			media = append(media, f)
		case KindSidecar:
			rep.Sidecars++
		case KindUnhandled:
			rep.Files = append(rep.Files, FileResult{CardFile: f, State: StateUnhandled,
				Detail: "Framelog does not import this file type"})
		case KindIgnored:
			rep.Files = append(rep.Files, FileResult{CardFile: f, State: StateIgnored,
				Detail: "switched off in File Types"})
		}
	}

	results := make([]FileResult, len(media))
	workers := opts.Workers
	if workers <= 0 {
		workers = 2
	}

	// Pipeline: this goroutine reads the card sequentially; a small pool
	// re-hashes the library and backup copies concurrently, so the slowest
	// device (usually the card) sets the pace instead of the sum of all three.
	type job struct {
		i     int
		entry db.LibraryEntry
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	done := 0
	report := func(file string) {
		if opts.Progress == nil {
			return
		}
		// Called under the lock so callers get serialised callbacks and
		// never see done go backwards.
		progressMu.Lock()
		defer progressMu.Unlock()
		done++
		opts.Progress(done, len(media), file)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				checkDestinations(&results[j.i], j.entry, opts.OriginalsPath, backupRoot, rep.BackupState)
				report(filepath.Base(results[j.i].RelPath))
			}
		}()
	}

	var dbErr error
	for i, f := range media {
		r := &results[i]
		r.CardFile = f
		h, err := hasher.HashFile(f.AbsPath)
		if err != nil {
			r.State = StateCardUnreadable
			r.Detail = err.Error()
			report(filepath.Base(f.RelPath))
			continue
		}
		r.Hash = h
		entry, ok, err := db.LibraryEntryByHash(opts.DB, h)
		if err != nil {
			dbErr = err
			break
		}
		if !ok {
			r.State = StateNotInLibrary
			r.Detail = "not imported — check inbox/ and inbox/failed/"
			report(filepath.Base(f.RelPath))
			continue
		}
		r.LibraryPath = entry.ImportedPath
		r.CaptureDate = entry.CaptureDate
		r.CameraModel = entry.CameraModel
		copied := opts.CopiedThisSession == nil || opts.CopiedThisSession[f.RelPath]
		if ts, err := time.Parse(time.RFC3339, entry.ImportTimestamp); err == nil && !sessionStart.IsZero() && copied {
			// import_timestamp has second resolution; truncate the session
			// start the same way so a file imported in the session's first
			// second still counts as new.
			r.New = !ts.Before(sessionStart.Truncate(time.Second))
		}
		jobs <- job{i: i, entry: entry}
	}
	close(jobs)
	wg.Wait()
	if dbErr != nil {
		return nil, fmt.Errorf("cardverify: catalog lookup: %w", dbErr)
	}

	rep.Files = append(results, rep.Files...)
	sort.SliceStable(rep.Files, func(i, j int) bool { return rep.Files[i].RelPath < rep.Files[j].RelPath })
	rep.FinishedAt = time.Now()
	rep.finalise(opts.ExtraProblems)
	return rep, nil
}

// checkDestinations re-hashes the library copy and (when the backup is
// reachable) the backup copy of one card file whose catalog row was found.
// r.Hash is the card's digest — the ground truth both copies must match.
func checkDestinations(r *FileResult, entry db.LibraryEntry, originals, backupRoot, backupState string) {
	libState, libDetail := compareCopy(entry.ImportedPath, r.Size, r.Hash)
	switch libState {
	case copyMissing:
		r.State, r.Detail = StateLibraryMissing, libDetail
		return
	case copyMismatch:
		r.State, r.Detail = StateLibraryMismatch, libDetail
		return
	}

	switch backupState {
	case BackupNotConfigured, BackupDriveMissing:
		r.State = StateVerified
		return
	}

	rel, err := filepath.Rel(originals, entry.ImportedPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		r.State, r.Detail = StateBackupMissing, "library file is outside originals/, so it is never backed up"
		return
	}
	r.BackupPath = filepath.Join(backupRoot, rel)
	bState, bDetail := compareCopy(r.BackupPath, r.Size, r.Hash)
	switch bState {
	case copyMissing:
		r.State, r.Detail = StateBackupMissing, bDetail
	case copyMismatch:
		r.State, r.Detail = StateBackupMismatch, bDetail
	default:
		r.State = StateVerified
	}
}

type copyState int

const (
	copyOK copyState = iota
	copyMissing
	copyMismatch
)

// compareCopy checks size first (cheap, catches truncation without reading
// the file) and then the full SHA-256.
func compareCopy(path string, size int64, wantHash string) (copyState, string) {
	fi, err := os.Stat(path)
	if err != nil {
		return copyMissing, err.Error()
	}
	if fi.Size() != size {
		return copyMismatch, fmt.Sprintf("size %d, card has %d", fi.Size(), size)
	}
	h, err := hasher.HashFile(path)
	if err != nil {
		return copyMismatch, "read error: " + err.Error()
	}
	if h != wantHash {
		return copyMismatch, "checksum differs from the card"
	}
	return copyOK, ""
}

// finalise computes BackupState (when the backup was reachable), the
// SafeToFormat verdict, and the human-readable Problems/Notes.
func (r *Report) finalise(extra []string) {
	counts := r.StateCounts()
	mediaTotal := r.MediaCount()

	if r.BackupState == "" {
		if counts[StateBackupMissing]+counts[StateBackupMismatch] > 0 {
			r.BackupState = BackupIncomplete
		} else {
			r.BackupState = BackupVerified
		}
	}

	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	add := func(n int, msg string) {
		if n > 0 {
			r.Problems = append(r.Problems, msg)
		}
	}
	add(counts[StateCardUnreadable], plural(counts[StateCardUnreadable], "file", "files")+
		" could not be read from the card — the card or reader may be failing")
	add(counts[StateNotInLibrary], plural(counts[StateNotInLibrary], "file is", "files are")+
		" not in the library yet — run ingest, or check inbox/failed/")
	add(counts[StateLibraryMissing], plural(counts[StateLibraryMissing], "library file is", "library files are")+
		" missing from originals/")
	add(counts[StateLibraryMismatch], plural(counts[StateLibraryMismatch], "library copy does", "library copies do")+
		" not match the card byte-for-byte (corrupt copy)")
	add(counts[StateBackupMissing], plural(counts[StateBackupMissing], "file is", "files are")+
		" not on the backup drive yet — use Retry Backup")
	add(counts[StateBackupMismatch], plural(counts[StateBackupMismatch], "backup copy does", "backup copies do")+
		" not match the card byte-for-byte (corrupt backup)")
	if n := counts[StateUnhandled]; n > 0 {
		example := ""
		for _, f := range r.Files {
			if f.State == StateUnhandled {
				example = filepath.Base(f.RelPath)
				break
			}
		}
		r.Problems = append(r.Problems, fmt.Sprintf(
			"%s on the card %s a type Framelog doesn't import (e.g. %s) — copy %s manually, or add the type in File Types",
			plural(n, "file", "files"), pick(n, "is", "are"), example, pick(n, "it", "them")))
	}
	if r.BackupState == BackupDriveMissing && mediaTotal > 0 {
		r.Problems = append(r.Problems,
			"Backup drive is not connected — the library is the only copy. Connect it and Verify Card again")
	}
	r.Problems = append(r.Problems, extra...)

	if r.BackupState == BackupNotConfigured && mediaTotal > 0 {
		r.Notes = append(r.Notes, "No backup drive is configured — the library is the only copy of these files")
	}
	if n := counts[StateIgnored]; n > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"%s not imported because %s switched off in File Types (%s). Formatting will delete %s",
			plural(n, "file was", "files were"), pick(n, "its type is", "their types are"),
			r.ignoredTypes(), pick(n, "it", "them")))
	}
	if mediaTotal == 0 && counts[StateUnhandled] == 0 {
		r.Notes = append(r.Notes, "No photos or videos found on the card")
	}

	verified := counts[StateVerified]
	r.SafeToFormat = verified == mediaTotal &&
		counts[StateUnhandled] == 0 &&
		r.BackupState != BackupDriveMissing &&
		r.BackupState != BackupIncomplete
}

// pick chooses the singular or plural wording for n.
func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ignoredTypes renders the switched-off types found on the card, e.g. "JPG 312".
func (r *Report) ignoredTypes() string {
	types := map[string]int{}
	for _, f := range r.Files {
		if f.State == StateIgnored {
			types[strings.ToUpper(strings.TrimPrefix(filepath.Ext(f.RelPath), "."))]++
		}
	}
	return formatTypes(types)
}

// StateCounts tallies Files by State.
func (r *Report) StateCounts() map[string]int {
	m := make(map[string]int)
	for _, f := range r.Files {
		m[f.State]++
	}
	return m
}

// MediaCount is the number of importable files found on the card.
func (r *Report) MediaCount() int {
	n := 0
	for _, f := range r.Files {
		if f.Kind == KindMedia {
			n++
		}
	}
	return n
}

// Summary flattens the report into the catalog row shape (also what the
// status IPC command reports as "last_card").
func (r *Report) Summary() db.CardReport {
	counts := r.StateCounts()
	var bytes int64
	types := map[string]int{}
	models := map[string]bool{}
	var first, last string
	newN, existing := 0, 0
	for _, f := range r.Files {
		if f.Kind != KindMedia {
			continue
		}
		bytes += f.Size
		types[strings.ToUpper(strings.TrimPrefix(filepath.Ext(f.RelPath), "."))]++
		if f.CameraModel != "" {
			models[f.CameraModel] = true
		}
		if f.CaptureDate != "" {
			if first == "" || f.CaptureDate < first {
				first = f.CaptureDate
			}
			if f.CaptureDate > last {
				last = f.CaptureDate
			}
		}
		if f.LibraryPath != "" {
			if f.New {
				newN++
			} else {
				existing++
			}
		}
	}
	media := r.MediaCount()
	return db.CardReport{
		VolumeName:    r.VolumeName,
		VolumeUUID:    r.VolumeUUID,
		StartedAt:     r.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:    r.FinishedAt.UTC().Format(time.RFC3339),
		FilesTotal:    media,
		BytesTotal:    bytes,
		FilesVerified: counts[StateVerified],
		FilesProblem:  len(r.Files) - counts[StateVerified] - counts[StateIgnored],
		FilesIgnored:  counts[StateIgnored],
		FilesNew:      newN,
		FilesExisting: existing,
		FileTypes:     formatTypes(types),
		CameraModels:  strings.Join(sortedKeys(models), ", "),
		CaptureFirst:  first,
		CaptureLast:   last,
		BackupState:   r.BackupState,
		SafeToFormat:  r.SafeToFormat,
		Problems:      r.Problems,
	}
}

// formatTypes renders {"ARW": 312, "MP4": 4} as "ARW 312 · MP4 4", largest
// count first so the headline format leads.
func formatTypes(types map[string]int) string {
	keys := sortedKeys(types)
	sort.SliceStable(keys, func(i, j int) bool { return types[keys[i]] > types[keys[j]] })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, types[k])
	}
	return strings.Join(parts, " · ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Save writes the plain-text receipt into reportsDir and records the report
// (summary + per-file rows) in the catalog. Returns the stored summary with
// ID and ReportPath filled in. A receipt write failure is not fatal — the
// DB row is the source of truth — so it is returned as a Note instead.
func Save(conn *sql.DB, r *Report, reportsDir string) (db.CardReport, error) {
	sum := r.Summary()
	if reportsDir != "" {
		path, err := WriteReceipt(r, reportsDir)
		if err != nil {
			r.Notes = append(r.Notes, "could not write report file: "+err.Error())
		} else {
			sum.ReportPath = path
		}
	}
	files := make([]db.CardReportFile, len(r.Files))
	for i, f := range r.Files {
		files[i] = db.CardReportFile{CardPath: f.RelPath, Size: f.Size, Hash: f.Hash,
			LibraryPath: f.LibraryPath, BackupPath: f.BackupPath, State: f.State, Detail: f.Detail}
	}
	id, err := db.InsertCardReport(conn, sum, files)
	if err != nil {
		return sum, fmt.Errorf("cardverify: save report: %w", err)
	}
	sum.ID = id
	return sum, nil
}

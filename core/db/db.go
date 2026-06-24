// Package db manages the SQLite catalog that is the single source of truth for
// the photo library. Schema is frozen in PROTOCOL.md §1 — any change there
// must land in the same commit as a change here.
//
// Driver choice: github.com/mattn/go-sqlite3 (CGo binding to the SQLite
// amalgamation). The pure-Go alternative (modernc.org/sqlite) avoids CGo but
// adds a ~4 MB binary and is less battle-tested at WAL concurrency. CGo is
// already acceptable here — the core binary is macOS-only and built via a
// normal toolchain, never cross-compiled. If that changes, swap drivers behind
// this package boundary without touching callers.
package db

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
)

// hash8RE matches an underscore-prefixed 8-hex-char segment immediately before
// the file extension, e.g. "_a1b2c3d4" in "20260622_140311_a1b2c3d4.jpg".
// This is the hash prefix ingest embeds in every originals filename (PROTOCOL.md §1).
var hash8RE = regexp.MustCompile(`(?i)_([0-9a-f]{8})\.[^.]+$`)

// ExtractHashPrefix returns the 8-hex-char hash segment embedded in a filename
// (e.g. "20260622_140311_a1b2c3d4.jpg" -> "a1b2c3d4", true), or ("", false) if
// no such segment is present. Filenames without one are a normal case — callers
// should treat false as "nothing to do here," not log it as a problem.
// The returned prefix is always lower-cased for consistent DB lookups.
func ExtractHashPrefix(filename string) (string, bool) {
	m := hash8RE.FindStringSubmatch(filename)
	if len(m) < 2 {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// Photo mirrors one row in the photos table. Fields map 1-to-1 with the frozen
// schema; callers set only what they have and leave the rest zero/nil.
// Nullable columns use pointer types so absent data is nil, not a zero value
// that could be mistaken for real coordinates or an unknown model string.
type Photo struct {
	Hash             string
	OriginalFilename string
	ImportedPath     string
	CameraModel      *string  // nil → NULL; populated from exif.Metadata.CameraModel
	CaptureDate      string
	ImportTimestamp  string
	GPSLat           *float64 // nil → NULL; not 0.0, which is a valid coordinate
	GPSLon           *float64
	Status           string // defaults to "raw" if empty (see InsertPhoto)
}

// Open returns a *sql.DB pointed at path. WAL mode and a 2 s busy_timeout are
// applied immediately on every connection so concurrent readers never block the
// writer for more than that window (FL-004). When readOnly is true the
// connection uses SQLite's immutable=1 + mode=ro query parameters, which cause
// the driver itself to reject any write attempt — this is what lets the Swift
// frontend hold a safe read-only handle (FL-403).
func Open(path string, readOnly bool) (*sql.DB, error) {
	var dsn string
	if readOnly {
		dsn = fmt.Sprintf("file:%s?mode=ro&immutable=1&_busy_timeout=2000", path)
	} else {
		dsn = fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=2000", path)
	}

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	// Verify the connection is actually live and pragmas applied.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// WAL mode must be re-applied after every new connection on the write path
	// because the query-parameter form only works on the first connection to
	// some driver versions. Belt-and-suspenders.
	if !readOnly {
		if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
			db.Close()
			return nil, err
		}
		if _, err := db.Exec("PRAGMA busy_timeout=2000;"); err != nil {
			db.Close()
			return nil, err
		}
	}

	// Limit the pool to one writer connection so SQLite's single-writer model
	// is never violated within the process. Readers can share freely.
	if !readOnly {
		db.SetMaxOpenConns(1)
	}
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(time.Minute)

	return db, nil
}

// InitDB creates the photos table if it does not exist, using the exact schema
// from PROTOCOL.md §1. Safe to call on every startup — CREATE TABLE IF NOT
// EXISTS is idempotent.
func InitDB(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS photos (
    hash              TEXT PRIMARY KEY,
    original_filename TEXT,
    imported_path     TEXT,
    camera_model      TEXT,
    capture_date      TEXT,
    import_timestamp  TEXT,
    gps_lat           REAL,
    gps_lon           REAL,
    status            TEXT DEFAULT 'raw'
);`
	_, err := db.Exec(schema)
	return err
}

// HashExists reports whether a row with the given hash already exists. Used by
// ingest (FL-201) to skip duplicates without re-hashing the full file.
func HashExists(db *sql.DB, hash string) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(1) FROM photos WHERE hash = ?", hash).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// InsertPhoto adds a new row to the photos table. If p.Status is empty it is
// set to "raw" (the PROTOCOL.md §1 default; belt-and-suspenders over the SQL
// DEFAULT so callers don't need to think about it).
func InsertPhoto(db *sql.DB, p Photo) error {
	if p.Status == "" {
		p.Status = "raw"
	}
	_, err := db.Exec(`
INSERT INTO photos
    (hash, original_filename, imported_path, camera_model, capture_date,
     import_timestamp, gps_lat, gps_lon, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Hash, p.OriginalFilename, p.ImportedPath, p.CameraModel,
		p.CaptureDate, p.ImportTimestamp, p.GPSLat, p.GPSLon, p.Status,
	)
	return err
}

// Status values are the closed set defined in PROTOCOL.md §1. Use these
// constants instead of bare strings so a typo is a compile error, not a
// silent wrong-status write.
const (
	StatusRaw       = "raw"
	StatusEdited    = "edited"
	StatusPublished = "published"
)

// UpdateStatus sets the status column for the row identified by hash. The only
// valid values per PROTOCOL.md §1 are "raw", "edited", and "published". Call
// sites: XMP watcher → "edited" (FL-204), outgest → "published" (FL-202).
func UpdateStatus(db *sql.DB, hash, status string) error {
	res, err := db.Exec("UPDATE photos SET status = ? WHERE hash = ?", status, hash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("db: UpdateStatus: no row with hash %q", hash)
	}
	return nil
}

// UpdateStatusByHashPrefix sets status on the row whose hash starts with
// hashPrefix (the 8-char prefix embedded in outgest filenames). Returns true
// when a row was matched and updated, false when no row matched — a false
// return is not an error (the file may have been imported before the DB
// existed, or the name may not correspond to any tracked photo).
// Call site: outgest → StatusPublished (FL-202).
func UpdateStatusByHashPrefix(db *sql.DB, hashPrefix, status string) (bool, error) {
	res, err := db.Exec("UPDATE photos SET status = ? WHERE hash LIKE ?",
		status, hashPrefix+"%")
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PhotoCount returns the total number of rows in the photos table. Used by the
// status handler (FL-302) and the Swift status display (FL-403).
func PhotoCount(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(1) FROM photos").Scan(&n)
	return n, err
}

// LastImport returns the maximum import_timestamp across all rows, or an empty
// string if the table is empty. Used by the status handler (FL-302) and the
// Swift polling loop (FL-403/FL-405) to detect whether a new ingest occurred.
func LastImport(db *sql.DB) (string, error) {
	var ts sql.NullString
	err := db.QueryRow("SELECT MAX(import_timestamp) FROM photos").Scan(&ts)
	if err != nil {
		return "", err
	}
	return ts.String, nil
}

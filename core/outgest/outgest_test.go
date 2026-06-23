package outgest

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/logging"
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

// fakeExiftool returns a shell body that emits CaptureDate for the given date.
func fakeExiftool(date string) string {
	return fmt.Sprintf(`echo '[{"DateTimeOriginal":"%s"}]'`, date)
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	conn, err := db.Open(path, false)
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

// newPipeline creates a test Pipeline with a fake exiftool reporting date.
// processed is returned so callers can populate it.
func newPipeline(t *testing.T, exiftoolDate string) (*Pipeline, string) {
	t.Helper()
	binDir := t.TempDir()
	processed := t.TempDir()
	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftool(exiftoolDate))
	conn := openTestDB(t)
	logger, _ := openTestLogger(t)
	return &Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}, processed
}

// writeFile creates name inside dir with content.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
	return p
}

// ---- tests ------------------------------------------------------------------

const testDate = "2026:06:22 14:03:11"

// TestOrganizeFile_WithHash8_StatusPublished is the core acceptance test:
// a file whose name contains an 8-hex hash prefix is moved to the correct
// YYYY/MM subdirectory AND the matching DB row's status becomes "published".
func TestOrganizeFile_WithHash8_StatusPublished(t *testing.T) {
	p, processed := newPipeline(t, testDate)

	// Insert a row with a known full hash whose first 8 chars appear in the filename.
	const fullHash = "a1b2c3d4e5f6789a"
	if err := db.InsertPhoto(p.DB, db.Photo{Hash: fullHash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	// Filename embeds the first 8 chars: 20260622_140311_a1b2c3d4.jpg
	src := writeFile(t, processed, "20260622_140311_a1b2c3d4.jpg", "fake export")

	result, err := p.OrganizeFile(src)
	if err != nil {
		t.Fatalf("OrganizeFile: %v", err)
	}
	if result != ResultMoved {
		t.Errorf("result = %q, want %q", result, ResultMoved)
	}

	// Source must be gone from processed/ root.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source still present at original path after move")
	}

	// Dest must exist at processed/2026/06/<filename>.
	wantDest := filepath.Join(processed, "2026", "06", "20260622_140311_a1b2c3d4.jpg")
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("dest not found at %s: %v", wantDest, err)
	}

	// DB row must now be "published".
	var status string
	if err := p.DB.QueryRow("SELECT status FROM photos WHERE hash = ?", fullHash).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != db.StatusPublished {
		t.Errorf("status = %q, want %q", status, db.StatusPublished)
	}
}

// TestOrganizeFile_NoHash8_StillMoves confirms a file without an embedded hash
// (e.g. a Lightroom default export name) is moved correctly and no DB row is
// touched — that's a normal case, not an error.
func TestOrganizeFile_NoHash8_StillMoves(t *testing.T) {
	conn := openTestDB(t)
	// Insert a sentinel row; its status must not change.
	if err := db.InsertPhoto(conn, db.Photo{Hash: "sentinelsentinel"}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	processed := t.TempDir()
	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftool(testDate))
	logger, _ := openTestLogger(t)

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	// Lightroom default-style name — no _XXXXXXXX segment.
	src := writeFile(t, processed, "IMG_001-Edit.jpg", "fake export")

	result, err := p.OrganizeFile(src)
	if err != nil {
		t.Fatalf("OrganizeFile: %v", err)
	}
	if result != ResultMoved {
		t.Errorf("result = %q, want %q", result, ResultMoved)
	}

	// File must be at the new location.
	wantDest := filepath.Join(processed, "2026", "06", "IMG_001-Edit.jpg")
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("dest not found: %v", err)
	}

	// Sentinel row must still be "raw" — nothing touched it.
	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = 'sentinelsentinel'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.StatusRaw {
		t.Errorf("sentinel status = %q, want %q", status, db.StatusRaw)
	}
}

// TestOrganizeFile_AlreadyOrganized checks idempotency: when the dest file
// already exists, OrganizeFile returns ResultSkipped without moving or touching
// the DB.
func TestOrganizeFile_AlreadyOrganized(t *testing.T) {
	conn := openTestDB(t)
	const fullHash = "bbbbbbbb12345678"
	if err := db.InsertPhoto(conn, db.Photo{Hash: fullHash}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	processed := t.TempDir()
	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftool(testDate))
	logger, _ := openTestLogger(t)

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	src := writeFile(t, processed, "20260622_140311_bbbbbbbb.jpg", "content")

	// Pre-create the dest so it already looks organized.
	destDir := filepath.Join(processed, "2026", "06")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, destDir, "20260622_140311_bbbbbbbb.jpg", "already here")

	result, err := p.OrganizeFile(src)
	if err != nil {
		t.Fatalf("OrganizeFile: %v", err)
	}
	if result != ResultSkipped {
		t.Errorf("result = %q, want %q", result, ResultSkipped)
	}

	// Source must still be untouched at its original path.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source missing after skip: %v", err)
	}

	// DB status must remain "raw" — skip means no status update.
	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", fullHash).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.StatusRaw {
		t.Errorf("status = %q after skip, want %q", status, db.StatusRaw)
	}
}

// TestRunOutgest_LogSummaryFormat reads the log file after a RunOutgest and
// asserts the summary line matches the PROTOCOL.md §5 format.
func TestRunOutgest_LogSummaryFormat(t *testing.T) {
	binDir := t.TempDir()
	processed := t.TempDir()
	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftool(testDate))
	conn := openTestDB(t)
	logger, logPath := openTestLogger(t)

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	writeFile(t, processed, "20260622_140311_cafebabe.jpg", "export")

	if _, err := p.RunOutgest(); err != nil {
		t.Fatalf("RunOutgest: %v", err)
	}
	if err := p.Logger.Close(); err != nil {
		t.Fatalf("logger.Close: %v", err)
	}

	summaryRe := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[OUTGEST\] Done: \d+ moved, \d+ skipped, \d+ failed$`,
	)
	found := false
	for _, l := range readLines(t, logPath) {
		if summaryRe.MatchString(l) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no summary line matching format in log; got:\n%s",
			strings.Join(readLines(t, logPath), "\n"))
	}
}

func TestTryAcquireRelease(t *testing.T) {
	p := &Pipeline{}

	if !p.TryAcquire() {
		t.Fatal("first TryAcquire() = false, want true")
	}
	if p.TryAcquire() {
		t.Fatal("second TryAcquire() without Release = true, want false")
	}
	p.Release()
	if !p.TryAcquire() {
		t.Fatal("TryAcquire() after Release = false, want true")
	}
	p.Release()
}


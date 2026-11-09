package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	conn, err := Open(path, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := InitDB(conn); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return conn, path
}

func TestInitDB_Idempotent(t *testing.T) {
	conn, _ := openTestDB(t)
	// Second call must not error (CREATE TABLE IF NOT EXISTS).
	if err := InitDB(conn); err != nil {
		t.Fatalf("second InitDB call: %v", err)
	}
}

func TestHashExists_BeforeAndAfterInsert(t *testing.T) {
	conn, _ := openTestDB(t)
	const hash = "abc123"

	exists, err := HashExists(conn, hash)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected HashExists=false before insert")
	}

	if err := InsertPhoto(conn, Photo{Hash: hash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	exists, err = HashExists(conn, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected HashExists=true after insert")
	}
}

func TestInsertPhoto_DefaultStatusIsRaw(t *testing.T) {
	conn, _ := openTestDB(t)
	const hash = "deadbeef"

	// Insert with empty Status — must default to "raw".
	if err := InsertPhoto(conn, Photo{Hash: hash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", hash).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "raw" {
		t.Errorf("status = %q, want %q", status, "raw")
	}
}


func TestPhotoCountAndLastImport_Empty(t *testing.T) {
	conn, _ := openTestDB(t)

	n, err := PhotoCount(conn)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("PhotoCount on empty table = %d, want 0", n)
	}

	ts, err := LastImport(conn)
	if err != nil {
		t.Fatal(err)
	}
	if ts != "" {
		t.Errorf("LastImport on empty table = %q, want \"\"", ts)
	}
}

func TestPhotoCountAndLastImport_AfterInserts(t *testing.T) {
	conn, _ := openTestDB(t)

	photos := []Photo{
		{Hash: "h1", ImportTimestamp: "2026-06-20T10:00:00Z"},
		{Hash: "h2", ImportTimestamp: "2026-06-21T12:00:00Z"},
	}
	for _, p := range photos {
		if err := InsertPhoto(conn, p); err != nil {
			t.Fatalf("InsertPhoto: %v", err)
		}
	}

	n, err := PhotoCount(conn)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("PhotoCount = %d, want 2", n)
	}

	ts, err := LastImport(conn)
	if err != nil {
		t.Fatal(err)
	}
	if ts != "2026-06-21T12:00:00Z" {
		t.Errorf("LastImport = %q, want %q", ts, "2026-06-21T12:00:00Z")
	}
}

func TestUpdateStatusByHashPrefix_Matches(t *testing.T) {
	conn, _ := openTestDB(t)
	const fullHash = "a1b2c3d4e5f60001"
	if err := InsertPhoto(conn, Photo{Hash: fullHash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	matched, err := UpdateStatusByHashPrefix(conn, "a1b2c3d4", StatusPublished)
	if err != nil {
		t.Fatalf("UpdateStatusByHashPrefix: %v", err)
	}
	if !matched {
		t.Error("matched=false, want true when prefix matches a row")
	}

	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", fullHash).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusPublished {
		t.Errorf("status = %q, want %q", status, StatusPublished)
	}
}

func TestUpdateStatusByHashPrefix_NoMatch(t *testing.T) {
	conn, _ := openTestDB(t)
	if err := InsertPhoto(conn, Photo{Hash: "ffffffffffffffff"}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	matched, err := UpdateStatusByHashPrefix(conn, "00000000", StatusPublished)
	if err != nil {
		t.Fatalf("UpdateStatusByHashPrefix: %v", err)
	}
	if matched {
		t.Error("matched=true, want false when prefix matches no row")
	}
}

func TestExtractHashPrefix_Present(t *testing.T) {
	got, ok := ExtractHashPrefix("20260622_140311_a1b2c3d4.jpg")
	if !ok {
		t.Fatal("ExtractHashPrefix returned false, want true")
	}
	if got != "a1b2c3d4" {
		t.Errorf("ExtractHashPrefix = %q, want %q", got, "a1b2c3d4")
	}
}

func TestExtractHashPrefix_Absent(t *testing.T) {
	_, ok := ExtractHashPrefix("IMG_001-Edit.jpg")
	if ok {
		t.Error("ExtractHashPrefix returned true for filename without hash segment")
	}
}

func TestExtractHashPrefix_TooShort(t *testing.T) {
	// 6 hex chars is not 8 — must NOT match.
	_, ok := ExtractHashPrefix("20260622_140311_a1b2c3.jpg")
	if ok {
		t.Error("ExtractHashPrefix returned true for 6-char hex segment, want false")
	}
}

// TestReadOnlyRejectsWrites is the FL-004 acceptance test: open one read-write
// and one read-only connection to the same file, then assert that any write
// through the read-only handle returns an error. This is what lets the Swift
// frontend hold a safe handle without needing its own write guards.
func TestReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro_test.db")

	// Prime the file with the schema.
	rw, err := Open(path, false)
	if err != nil {
		t.Fatalf("Open rw: %v", err)
	}
	defer rw.Close()
	if err := InitDB(rw); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	// Open a second connection as read-only.
	ro, err := Open(path, true)
	if err != nil {
		t.Fatalf("Open ro: %v", err)
	}
	defer ro.Close()

	// Any write through the read-only handle must fail.
	err = InsertPhoto(ro, Photo{Hash: "should-fail"})
	if err == nil {
		t.Fatal("expected write through read-only connection to fail, got nil")
	}
}

func TestCardReport_RoundTrip(t *testing.T) {
	conn := openReportTestDB(t)

	if r, err := LastCardReport(conn); err != nil || r != nil {
		t.Fatalf("empty LastCardReport = %v, %v; want nil, nil", r, err)
	}

	in := CardReport{
		VolumeName: "Untitled", VolumeUUID: "UUID-1",
		StartedAt: "2026-11-07T10:00:00Z", FinishedAt: "2026-11-07T10:05:00Z",
		FilesTotal: 2, BytesTotal: 99, FilesVerified: 1, FilesProblem: 1,
		FilesNew: 1, FileTypes: "ARW 2", CameraModels: "ILCE-7CM2",
		CaptureFirst: "2026:11:06 18:00:00", CaptureLast: "2026:11:06 19:00:00",
		BackupState: "verified", SafeToFormat: false,
		Problems:   []string{"1 file is not in the library", "second line"},
		ReportPath: "/r.txt",
	}
	files := []CardReportFile{
		{CardPath: "DCIM/100MSDCF/DSC00001.ARW", Size: 50, Hash: "aa", State: "verified"},
		{CardPath: "DCIM/100MSDCF/DSC00002.ARW", Size: 49, Hash: "bb", State: "not_in_library"},
	}
	id, err := InsertCardReport(conn, in, files)
	if err != nil {
		t.Fatalf("InsertCardReport: %v", err)
	}

	got, err := LastCardReport(conn)
	if err != nil || got == nil {
		t.Fatalf("LastCardReport: %v, %v", got, err)
	}
	in.ID = id
	if got.ID != in.ID || got.VolumeName != in.VolumeName || got.SafeToFormat != in.SafeToFormat ||
		got.FilesProblem != 1 || len(got.Problems) != 2 || got.Problems[1] != "second line" ||
		got.ReportPath != "/r.txt" || got.BytesTotal != 99 || got.FilesNew != 1 ||
		got.FileTypes != "ARW 2" || got.CameraModels != "ILCE-7CM2" ||
		got.CaptureLast != "2026:11:06 19:00:00" {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, in)
	}

	var n int
	conn.QueryRow("SELECT COUNT(*) FROM card_report_files WHERE report_id = ?", id).Scan(&n)
	if n != 2 {
		t.Errorf("card_report_files rows = %d, want 2", n)
	}
}

func TestBackupRuns(t *testing.T) {
	conn := openReportTestDB(t)

	if r, err := LastBackupRun(conn); err != nil || r != nil {
		t.Fatalf("empty LastBackupRun = %v, %v", r, err)
	}
	if ts, err := LastSuccessfulBackupAt(conn); err != nil || ts != "" {
		t.Fatalf("empty LastSuccessfulBackupAt = %q, %v", ts, err)
	}

	must := func(r BackupRun) {
		t.Helper()
		if err := InsertBackupRun(conn, r); err != nil {
			t.Fatal(err)
		}
	}
	must(BackupRun{StartedAt: "a", FinishedAt: "2026-11-01T00:00:00Z", Outcome: BackupOK, FilesCopied: 4})
	must(BackupRun{StartedAt: "b", FinishedAt: "2026-11-02T00:00:00Z", Outcome: BackupFailed,
		FilesFailed: 2, Error: "Backup drive is full"})

	last, err := LastBackupRun(conn)
	if err != nil || last == nil {
		t.Fatal(err)
	}
	if last.Outcome != BackupFailed || last.Error != "Backup drive is full" || last.FilesFailed != 2 {
		t.Errorf("LastBackupRun = %+v", *last)
	}
	ts, _ := LastSuccessfulBackupAt(conn)
	if ts != "2026-11-01T00:00:00Z" {
		t.Errorf("LastSuccessfulBackupAt = %q", ts)
	}
}

func TestLibraryEntryByHash(t *testing.T) {
	conn := openReportTestDB(t)
	model := "ILCE-7CM2"
	if err := InsertPhoto(conn, Photo{Hash: "h1", ImportedPath: "/o/a.arw",
		ImportTimestamp: "2026-11-07T10:00:00Z", CaptureDate: "2026:11:06 18:00:00",
		CameraModel: &model}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := LibraryEntryByHash(conn, "h1")
	want := LibraryEntry{ImportedPath: "/o/a.arw", ImportTimestamp: "2026-11-07T10:00:00Z",
		CaptureDate: "2026:11:06 18:00:00", CameraModel: "ILCE-7CM2"}
	if err != nil || !ok || e != want {
		t.Errorf("LibraryEntryByHash(h1) = %+v, %v, %v", e, ok, err)
	}
	if _, ok, err := LibraryEntryByHash(conn, "nope"); err != nil || ok {
		t.Errorf("LibraryEntryByHash(nope) ok=%v err=%v", ok, err)
	}
}

func openReportTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := Open(filepath.Join(t.TempDir(), "c.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := InitDB(conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

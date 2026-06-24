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

func TestUpdateStatus(t *testing.T) {
	conn, _ := openTestDB(t)
	const hash = "cafebabe"

	if err := InsertPhoto(conn, Photo{Hash: hash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	for _, want := range []string{"edited", "published", "raw"} {
		if err := UpdateStatus(conn, hash, want); err != nil {
			t.Fatalf("UpdateStatus(%q): %v", want, err)
		}
		var got string
		if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", hash).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("after UpdateStatus(%q): got %q", want, got)
		}
	}
}

func TestUpdateStatus_MissingHash(t *testing.T) {
	conn, _ := openTestDB(t)
	if err := UpdateStatus(conn, "no-such-hash", "edited"); err == nil {
		t.Error("expected error for missing hash, got nil")
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

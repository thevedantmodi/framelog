package ingest

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
	"github.com/thevedantmodi/framelog/core/logging"
)

// ---- test helpers -----------------------------------------------------------

// writeFakeBin creates dir/<name> as an executable shell script running body.
func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

// fakeExiftoolScript returns a shell body that emits one JSON record with the
// given model, capture date, and GPS coordinates. All callers control what the
// fake reports so tests are deterministic and require no real exiftool.
func fakeExiftoolScript(model, date string, lat, lon float64) string {
	return fmt.Sprintf(
		`echo '[{"Model":"%s","DateTimeOriginal":"%s","GPSLatitude":%f,"GPSLongitude":%f}]'`,
		model, date, lat, lon,
	)
}

// fakeExiftoolNoGPS emits a record with no GPS fields at all.
func fakeExiftoolNoGPS(model, date string) string {
	return fmt.Sprintf(`echo '[{"Model":"%s","DateTimeOriginal":"%s"}]'`, model, date)
}

// writePhoto creates a small test file at inbox/<name>.
func writePhoto(t *testing.T, inbox, name string) string {
	t.Helper()
	p := filepath.Join(inbox, name)
	if err := os.WriteFile(p, []byte("fake raw: "+name), 0o644); err != nil {
		t.Fatalf("writePhoto: %v", err)
	}
	return p
}

// findGit returns the real git binary or skips the test.
func findGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	return git
}

// initGitRepo runs git init and sets local user config in dir.
func initGitRepo(t *testing.T, git, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@framelog.test")
	run("config", "user.name", "Framelog Test")
}

// setupRepoWithRemote creates a git repo in repoDir with an initial commit and
// a bare repo in bareDir as its "origin" remote.
func setupRepoWithRemote(t *testing.T, git, repoDir, bareDir string) {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	initGitRepo(t, git, repoDir)
	run(bareDir, "init", "--bare")

	// Initial commit so the branch exists before we add a remote.
	if err := os.WriteFile(filepath.Join(repoDir, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(repoDir, "add", "-A")
	run(repoDir, "commit", "-m", "init")
	run(repoDir, "remote", "add", "origin", bareDir)
	run(repoDir, "push", "-u", "origin", "HEAD")
}

// openTestDB opens an in-memory-ish SQLite DB (file in TempDir), inits the
// schema, and registers a cleanup close.
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

// openTestLogger creates a Logger writing to a temp file and registers cleanup.
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

// readLines returns all lines from path.
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

// newPipeline builds a Pipeline with fake exiftool / pmset and a real git repo
// in originals. It does NOT configure a bare remote — use newPipelineWithRemote
// for end-to-end push tests.
func newPipeline(t *testing.T, exiftoolBody string) (*Pipeline, string, string) {
	t.Helper()
	git := findGit(t)

	binDir := t.TempDir()
	inbox := t.TempDir()
	originals := t.TempDir()

	initGitRepo(t, git, originals)

	exiftool := writeFakeBin(t, binDir, "exiftool", exiftoolBody)
	pmset := writeFakeBin(t, binDir, "pmset", `echo "Now drawing from 'Battery Power'"`)

	conn := openTestDB(t)
	logger, logPath := openTestLogger(t)

	return &Pipeline{
		DB:            conn,
		Logger:        logger,
		InboxPath:     inbox,
		OriginalsPath: originals,
		ExiftoolPath:  exiftool,
		GitPath:       git,
		PmsetPath:     pmset,
	}, inbox, logPath
}

// ---- tests ------------------------------------------------------------------

const (
	testModel = "X-T5"
	testDate  = "2026:06:22 14:03:11"
	testLat   = 37.7749
	testLon   = -122.4194
)

// TestImportFile_FullImport_GPSFlowsToDatabase is the acceptance test for the
// FL-201 requirement: GPS coordinates read by exiftool must end up in the DB
// row. The Python predecessor read them but silently dropped them; this test
// would have caught that.
func TestImportFile_FullImport_GPSFlowsToDatabase(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	src := writePhoto(t, inbox, "photo.raf")
	hash, err := hasher.HashFile(src)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	result, err := p.ImportFile(src, "batch1")
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if result != ResultImported {
		t.Fatalf("result = %q, want %q", result, ResultImported)
	}

	// Source must be gone from inbox.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source file still present in inbox after successful import")
	}

	// Dest must exist at the exact expected path.
	wantDir := filepath.Join(p.OriginalsPath, "2026", "06", "22")
	wantFile := fmt.Sprintf("20260622_140311_%s.raf", hash[:8])
	wantDest := filepath.Join(wantDir, wantFile)
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("dest file not found at expected path %s: %v", wantDest, err)
	}

	// XMP sidecar must exist next to the dest.
	wantXMP := strings.TrimSuffix(wantDest, ".raf") + ".xmp"
	if _, err := os.Stat(wantXMP); err != nil {
		t.Errorf("XMP sidecar not found at %s: %v", wantXMP, err)
	}

	// DB row must contain the GPS coordinates from the fake exiftool output.
	var lat, lon sql.NullFloat64
	err = p.DB.QueryRow(
		"SELECT gps_lat, gps_lon FROM photos WHERE hash = ?", hash,
	).Scan(&lat, &lon)
	if err != nil {
		t.Fatalf("query gps: %v", err)
	}
	if !lat.Valid || lat.Float64 != testLat {
		t.Errorf("gps_lat = %v (valid=%v), want %v", lat.Float64, lat.Valid, testLat)
	}
	if !lon.Valid || lon.Float64 != testLon {
		t.Errorf("gps_lon = %v (valid=%v), want %v", lon.Float64, lon.Valid, testLon)
	}
}

func TestImportFile_Dedup(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	src := writePhoto(t, inbox, "photo.raf")

	// First import: succeeds.
	if r, err := p.ImportFile(src, "b1"); err != nil || r != ResultImported {
		t.Fatalf("first import: result=%q err=%v", r, err)
	}

	// Re-create the same-content file in inbox (first import removed it).
	src2 := writePhoto(t, inbox, "photo_copy.raf")
	// Write same content so hash matches.
	if err := os.WriteFile(src2, []byte("fake raw: photo.raf"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := p.ImportFile(src2, "b2")
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if r != ResultSkipped {
		t.Errorf("second import result = %q, want %q", r, ResultSkipped)
	}

	n, err := db.PhotoCount(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("photo_count = %d, want 1 after duplicate import", n)
	}

	// The duplicate source must be parked in inbox/duplicates/, not left in
	// the inbox root where every future run would re-hash and re-skip it.
	if _, err := os.Stat(src2); !os.IsNotExist(err) {
		t.Error("duplicate source still in inbox root after skip")
	}
	parked := filepath.Join(inbox, "duplicates", "photo_copy.raf")
	if _, err := os.Stat(parked); err != nil {
		t.Errorf("duplicate not parked at %s: %v", parked, err)
	}
}

// TestImportFile_DuplicateNameCollisionGetsSuffix: two different duplicates
// with the same basename (e.g. IMG_0001.JPG from two card folders) must both
// survive the move into inbox/duplicates/.
func TestImportFile_DuplicateNameCollisionGetsSuffix(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	src := writePhoto(t, inbox, "photo.raf")
	if r, err := p.ImportFile(src, "b1"); err != nil || r != ResultImported {
		t.Fatalf("first import: result=%q err=%v", r, err)
	}

	// Occupy the parked name, then import another duplicate with that basename.
	if err := os.MkdirAll(filepath.Join(inbox, "duplicates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "duplicates", "photo.raf"), []byte("earlier dup"), 0o644); err != nil {
		t.Fatal(err)
	}
	src2 := writePhoto(t, inbox, "photo.raf")
	if err := os.WriteFile(src2, []byte("fake raw: photo.raf"), 0o644); err != nil {
		t.Fatal(err)
	}

	if r, err := p.ImportFile(src2, "b2"); err != nil || r != ResultSkipped {
		t.Fatalf("duplicate import: result=%q err=%v", r, err)
	}
	suffixed := filepath.Join(inbox, "duplicates", "photo-1.raf")
	if _, err := os.Stat(suffixed); err != nil {
		t.Errorf("colliding duplicate not parked at %s: %v", suffixed, err)
	}
}

// TestRunIngest_SkipsDuplicatesDir: files parked in inbox/duplicates/ must be
// invisible to the ingest walk — no re-hash, no skip tally, no removal.
func TestRunIngest_SkipsDuplicatesDir(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	dupDir := filepath.Join(inbox, "duplicates")
	if err := os.MkdirAll(dupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(dupDir, "old.raf")
	if err := os.WriteFile(parked, []byte("parked duplicate"), 0o644); err != nil {
		t.Fatal(err)
	}

	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts != (Counts{}) {
		t.Errorf("counts = %+v, want all zero — duplicates/ must not be scanned", counts)
	}
	if _, err := os.Stat(parked); err != nil {
		t.Errorf("parked file disturbed by RunIngest: %v", err)
	}
}

func TestRunIngest_MixedExtensions(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	// 3 supported files, 1 unsupported.
	writePhoto(t, inbox, "a.raf")
	writePhoto(t, inbox, "b.cr3")
	writePhoto(t, inbox, "c.jpg")
	unsupported := writePhoto(t, inbox, "d.txt")

	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts.Imported != 3 {
		t.Errorf("Imported = %d, want 3", counts.Imported)
	}
	if counts.Failed != 0 {
		t.Errorf("Failed = %d, want 0", counts.Failed)
	}

	// Unsupported file must be completely untouched.
	if _, err := os.Stat(unsupported); err != nil {
		t.Errorf("unsupported file was removed from inbox: %v", err)
	}
}

func TestRunIngest_EmptyInbox(t *testing.T) {
	git := findGit(t)

	binDir := t.TempDir()
	inbox := t.TempDir()
	originals := t.TempDir()

	initGitRepo(t, git, originals)

	// Give it an initial commit so git log works.
	exec.Command(git, "-C", originals, "commit", "--allow-empty", "-m", "init").Run()

	exiftool := writeFakeBin(t, binDir, "exiftool", fakeExiftoolNoGPS(testModel, testDate))
	pmset := writeFakeBin(t, binDir, "pmset", `echo "Battery Power"`)

	conn := openTestDB(t)
	logger, _ := openTestLogger(t)
	t.Cleanup(func() { logger.Close() })

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		InboxPath:     inbox,
		OriginalsPath: originals,
		ExiftoolPath:  exiftool,
		GitPath:       git,
		PmsetPath:     pmset,
	}

	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts != (Counts{}) {
		t.Errorf("counts = %+v, want all zeros", counts)
	}

	// No new commit should have been made — git log should still show only "init".
	out, err := exec.Command(git, "-C", originals, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		t.Errorf("git log shows %d commits after empty ingest, want 1: %v", len(lines), lines)
	}
}

// TestRunIngest_LogSummaryFormat reads the log file after a RunIngest and
// confirms the summary line matches the format frozen in PROTOCOL.md §5.
func TestRunIngest_LogSummaryFormat(t *testing.T) {
	p, inbox, logPath := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	writePhoto(t, inbox, "photo.raf")

	if _, err := p.RunIngest(); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	// Must close logger to flush before reading.
	if err := p.Logger.Close(); err != nil {
		t.Fatalf("logger.Close: %v", err)
	}

	lines := readLines(t, logPath)

	// PROTOCOL.md §5: "2026-06-22 14:03:11 [INGEST] Done: 3 imported, 1 skipped, 0 failed"
	summaryRe := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[INGEST\] Done: \d+ imported, \d+ skipped, \d+ failed$`,
	)
	found := false
	for _, l := range lines {
		if summaryRe.MatchString(l) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no summary line matching format in log; got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestRunIngest_EndToEnd_GitPush is the full end-to-end test: real git repo +
// bare remote, fake pmset reporting AC power, two files imported. Asserts the
// commit message format and that the commit landed in the bare remote.
func TestRunIngest_EndToEnd_GitPush(t *testing.T) {
	git := findGit(t)

	binDir := t.TempDir()
	inbox := t.TempDir()
	originals := t.TempDir()
	bare := t.TempDir()

	setupRepoWithRemote(t, git, originals, bare)

	exiftool := writeFakeBin(t, binDir, "exiftool",
		fakeExiftoolScript(testModel, testDate, testLat, testLon))
	// AC power → push will be attempted.
	pmset := writeFakeBin(t, binDir, "pmset", `echo "Now drawing from 'AC Power'"`)

	conn := openTestDB(t)
	logger, _ := openTestLogger(t)
	t.Cleanup(func() { logger.Close() })

	p := &Pipeline{
		DB:            conn,
		Logger:        logger,
		InboxPath:     inbox,
		OriginalsPath: originals,
		ExiftoolPath:  exiftool,
		GitPath:       git,
		PmsetPath:     pmset,
	}

	writePhoto(t, inbox, "alpha.raf")
	writePhoto(t, inbox, "beta.raf")

	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts.Imported != 2 {
		t.Errorf("Imported = %d, want 2", counts.Imported)
	}

	// Commit message must be "ingest: YYYY-MM-DD (2 photos)".
	msgRe := regexp.MustCompile(`^ingest: \d{4}-\d{2}-\d{2} \(2 photos\)$`)
	out, err := exec.Command(git, "-C", originals, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if !msgRe.MatchString(got) {
		t.Errorf("commit subject = %q, want format %q", got, "ingest: YYYY-MM-DD (2 photos)")
	}

	// Commit must have been pushed to the bare remote.
	remoteOut, err := exec.Command(git, "-C", bare, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log bare: %v", err)
	}
	if strings.TrimSpace(string(remoteOut)) != strings.TrimSpace(string(out)) {
		t.Errorf("bare remote HEAD = %q, want %q", strings.TrimSpace(string(remoteOut)), got)
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
	p.Release() // clean up
}

func TestPauseResume_BlocksAndUnblocksRunIngest(t *testing.T) {
	p := &Pipeline{}

	if p.Paused() {
		t.Fatal("Paused() = true before Pause() called")
	}

	p.Pause()
	if !p.Paused() {
		t.Fatal("Paused() = false after Pause()")
	}
	if _, err := p.RunIngest(); !errors.Is(err, ErrIngestPaused) {
		t.Fatalf("RunIngest() while paused = %v, want ErrIngestPaused", err)
	}

	// TryAcquire is unaffected by Pause — pausing blocks RunIngest at the top,
	// not the lower-level locking primitive tests rely on in isolation.
	if !p.TryAcquire() {
		t.Fatal("TryAcquire() while paused = false, want true")
	}
	p.Release()

	p.Resume()
	if p.Paused() {
		t.Fatal("Paused() = true after Resume()")
	}
}

// ---- backup tests -----------------------------------------------------------

// fakeRclone returns a fake rclone script that writes its arguments to
// argsFile and exits 0. argsFile is chosen by the caller so multiple
// tests can use independent sentinels without collision.
func fakeRclone(t *testing.T, binDir, argsFile string) string {
	t.Helper()
	return writeFakeBin(t, binDir, "rclone",
		`echo "$@" > `+argsFile)
}

// TestRunIngest_BackupInvoked proves backup.Sync is called with the correct
// arguments when imports succeed and BackupPath is configured. The default
// pmset in newPipeline reports battery power — this test also implicitly
// demonstrates that backup is not gated on AC power or git push.
func TestRunIngest_BackupInvoked(t *testing.T) {
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
		t.Fatalf("Imported = %d, want 1", counts.Imported)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("rclone was not invoked (sentinel missing): %v", err)
	}
	got := strings.TrimSpace(string(raw))
	for _, want := range []string{"copy", p.OriginalsPath, filepath.Join(backupPath, "originals")} {
		if !strings.Contains(got, want) {
			t.Errorf("rclone args %q does not contain %q", got, want)
		}
	}
}

// TestRunIngest_BackupDisabled asserts rclone is never called when BackupPath
// is the empty string (the "disabled" state from config.BackupPath when the
// env var is unset).
func TestRunIngest_BackupDisabled(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "rclone-args")
	p.RclonePath = fakeRclone(t, binDir, argsFile)
	p.BackupPath = "" // disabled

	writePhoto(t, inbox, "photo.raf")
	if _, err := p.RunIngest(); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("rclone was invoked despite BackupPath being empty")
	}
}

// TestRunIngest_BackupPathMissing asserts ingest still succeeds (Imported > 0,
// no error) when BackupPath is set but the directory does not exist. Sync's own
// short-circuit handles the absent drive; ingest must not check existence itself.
func TestRunIngest_BackupPathMissing(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "rclone-args")
	p.RclonePath = fakeRclone(t, binDir, argsFile)
	p.BackupPath = "/nonexistent/backup/drive/path"

	writePhoto(t, inbox, "photo.raf")
	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest returned error for missing backup path: %v", err)
	}
	if counts.Imported != 1 {
		t.Errorf("Imported = %d, want 1 (ingest must succeed regardless of backup state)", counts.Imported)
	}
	// rclone must not have been called — Sync short-circuited before the binary.
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Error("rclone was invoked for a non-existent backup path (Sync should short-circuit)")
	}
}


// TestImportFile_OnFileWrittenReportsDestAndSidecar asserts the hook main.go
// wires to xmpwatcher.Suppress receives every path ingest writes into
// originals/ — the copied photo and its XMP sidecar — so ingest's own writes
// never masquerade as Lightroom edits (which would flip status to "edited").
func TestImportFile_OnFileWrittenReportsDestAndSidecar(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))

	var written []string
	p.OnFileWritten = func(path string) { written = append(written, path) }

	src := writePhoto(t, inbox, "photo.raf")
	hash, err := hasher.HashFile(src)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if result, err := p.ImportFile(src, "batch1"); err != nil || result != ResultImported {
		t.Fatalf("ImportFile = %q, %v; want %q, nil", result, err, ResultImported)
	}

	base := filepath.Join(p.OriginalsPath, "2026", "06", "22",
		fmt.Sprintf("20260622_140311_%s", hash[:8]))
	want := []string{base + ".raf", base + ".xmp"}
	if len(written) != 2 || written[0] != want[0] || written[1] != want[1] {
		t.Errorf("OnFileWritten got %v, want %v", written, want)
	}
}

// TestRunIngest_QuarantinesRepeatedlyFailingFile: a file that fails import
// (exiftool error — same class as a zero-byte or corrupt file) retries for
// maxImportAttempts runs, then moves to inbox/failed/ and disappears from the
// scan. Without the quarantine it failed on every run forever.
func TestRunIngest_QuarantinesRepeatedlyFailingFile(t *testing.T) {
	p, inbox, _ := newPipeline(t, `echo "boom" >&2; exit 1`)
	poison := writePhoto(t, inbox, "poison.raf")

	for i := 1; i <= 3; i++ {
		counts, err := p.RunIngest()
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if counts.Failed != 1 {
			t.Fatalf("run %d: failed = %d, want 1", i, counts.Failed)
		}
	}

	// After the third failure the file must be quarantined...
	if _, err := os.Stat(poison); !os.IsNotExist(err) {
		t.Error("poison file still in inbox root after 3 failed runs")
	}
	quarantined := filepath.Join(inbox, "failed", "poison.raf")
	if _, err := os.Stat(quarantined); err != nil {
		t.Errorf("poison file not quarantined at %s: %v", quarantined, err)
	}

	// ...and the next run must not see it at all.
	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("run 4: %v", err)
	}
	if counts != (Counts{}) {
		t.Errorf("run 4 counts = %+v, want all zero — failed/ must not be scanned", counts)
	}
}

// TestRunIngest_SuccessClearsFailCount: a transient failure must not count
// toward quarantine once an attempt succeeds — only consecutive failures park
// a file.
func TestRunIngest_SuccessClearsFailCount(t *testing.T) {
	p, inbox, _ := newPipeline(t, fakeExiftoolScript(testModel, testDate, testLat, testLon))
	src := writePhoto(t, inbox, "photo.raf")

	// Two failures recorded directly (as if from two earlier runs).
	if _, err := p.fail(src, os.ErrPermission); err == nil {
		t.Fatal("fail returned nil error")
	}
	if _, err := p.fail(src, os.ErrPermission); err == nil {
		t.Fatal("fail returned nil error")
	}

	// A successful run imports the file and forgives the tally.
	counts, err := p.RunIngest()
	if err != nil {
		t.Fatalf("RunIngest: %v", err)
	}
	if counts.Imported != 1 {
		t.Fatalf("imported = %d, want 1", counts.Imported)
	}
	p.mu.Lock()
	_, tracked := p.failCounts[src]
	p.mu.Unlock()
	if tracked {
		t.Error("fail count for imported file not cleared — a later transient failure would quarantine too eagerly")
	}
}

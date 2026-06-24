package xmpwatcher

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "catalog.db"), false)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.InitDB(conn); err != nil {
		t.Fatalf("db.InitDB: %v", err)
	}
	return conn
}

func openTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(filepath.Join(t.TempDir(), "test.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func findGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	return git
}

func gitCmd(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// setupRepo creates a git repo in dir with an initial commit and local user config.
func setupRepo(t *testing.T, git, dir string) {
	t.Helper()
	gitCmd(t, git, dir, "init")
	gitCmd(t, git, dir, "config", "user.email", "test@framelog.test")
	gitCmd(t, git, dir, "config", "user.name", "Framelog Test")
	gitCmd(t, git, dir, "commit", "--allow-empty", "-m", "initial")
}

// setupRepoWithRemote creates a git repo + bare remote and sets up tracking.
func setupRepoWithRemote(t *testing.T, git, repoDir, bareDir string) {
	t.Helper()
	setupRepo(t, git, repoDir)
	gitCmd(t, git, bareDir, "init", "--bare")
	gitCmd(t, git, repoDir, "remote", "add", "origin", bareDir)
	gitCmd(t, git, repoDir, "push", "-u", "origin", "HEAD")
}

func gitLogCount(t *testing.T, git, dir string) int {
	t.Helper()
	out := gitCmd(t, git, dir, "log", "--oneline")
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// newWatcher builds a Watcher wired to a real git repo in originals with fake
// pmset (AC power) and fake pgrep (Lightroom not running).
func newWatcher(t *testing.T, git, originals string, debounce time.Duration) (*Watcher, string, string) {
	t.Helper()
	binDir := t.TempDir()
	pmset := writeFakeBin(t, binDir, "pmset", `echo "Now drawing from 'AC Power'"`)
	pgrep := writeFakeBin(t, binDir, "pgrep", `exit 1`) // Lightroom not running
	return &Watcher{
		GitPath:          git,
		OriginalsPath:    originals,
		PmsetPath:        pmset,
		PgrepPath:        pgrep,
		DB:               openTestDB(t),
		Logger:           openTestLogger(t),
		DebounceDuration: debounce,
	}, pmset, pgrep
}

// ---- FindPgrep --------------------------------------------------------------

func TestFindPgrep_LookPathFallback(t *testing.T) {
	orig := pgrepCandidates
	pgrepCandidates = []string{"/nonexistent/pgrep-a", "/nonexistent/pgrep-b"}
	t.Cleanup(func() { pgrepCandidates = orig })

	binDir := t.TempDir()
	writeFakeBin(t, binDir, "pgrep", `exit 1`)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	p, err := FindPgrep()
	if err != nil {
		t.Fatalf("FindPgrep: %v", err)
	}
	if p == "" {
		t.Fatal("FindPgrep returned empty path")
	}
}

func TestFindPgrep_NotFound(t *testing.T) {
	orig := pgrepCandidates
	pgrepCandidates = []string{"/nonexistent/pgrep-a"}
	t.Cleanup(func() { pgrepCandidates = orig })

	t.Setenv("PATH", t.TempDir()) // empty — nothing on PATH
	_, err := FindPgrep()
	if err == nil {
		t.Fatal("FindPgrep returned nil error when binary absent")
	}
}

// ---- IsLightroomRunning -----------------------------------------------------

func TestIsLightroomRunning_True(t *testing.T) {
	binDir := t.TempDir()
	fake := writeFakeBin(t, binDir, "pgrep", `exit 0`) // exit 0 = match found
	got, err := IsLightroomRunning(fake)
	if err != nil {
		t.Fatalf("IsLightroomRunning: %v", err)
	}
	if !got {
		t.Error("IsLightroomRunning = false, want true when pgrep exits 0")
	}
}

func TestIsLightroomRunning_False(t *testing.T) {
	binDir := t.TempDir()
	fake := writeFakeBin(t, binDir, "pgrep", `exit 1`) // exit 1 = no process matched
	got, err := IsLightroomRunning(fake)
	if err != nil {
		t.Fatalf("IsLightroomRunning: %v", err)
	}
	if got {
		t.Error("IsLightroomRunning = true, want false when pgrep exits 1")
	}
}

// ---- Debounce collapses burst -----------------------------------------------

// TestDebounce_CollapsesBurst fires 5 writes to 3 distinct files within a tight
// window and asserts exactly ONE git commit results, with a "(3 files)" message.
func TestDebounce_CollapsesBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)
	originals := t.TempDir()
	setupRepo(t, git, originals)

	w, _, _ := newWatcher(t, git, originals, 100*time.Millisecond)
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run() }()
	time.Sleep(200 * time.Millisecond) // let startup walk complete

	// 5 writes to 3 distinct .xmp files — debounce must collapse to 1 commit.
	fileA := filepath.Join(originals, "20260622_a1b2c3d4.xmp")
	fileB := filepath.Join(originals, "20260622_e5f67890.xmp")
	fileC := filepath.Join(originals, "20260622_ffffffff.xmp")
	for i, f := range []string{fileA, fileA, fileB, fileC, fileB} {
		if err := os.WriteFile(f, []byte(fmt.Sprintf("edit %d", i)), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Wait for debounce (100ms) + git commit time + margin.
	time.Sleep(800 * time.Millisecond)
	w.Stop()

	if n := gitLogCount(t, git, originals); n != 2 {
		t.Errorf("git log count = %d, want 2 (initial + 1 edit commit)", n)
	}

	subject := gitCmd(t, git, originals, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "(3 files)") {
		t.Errorf("commit subject = %q, want contains \"(3 files)\"", strings.TrimSpace(subject))
	}
}

// ---- Status update ----------------------------------------------------------

// TestStatusUpdate_EditedOnHash asserts that a file whose name embeds a hash8
// segment causes the matching DB row's status to become "edited" after the
// debounce fires.
func TestStatusUpdate_EditedOnHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)
	originals := t.TempDir()
	setupRepo(t, git, originals)

	const fullHash = "a1b2c3d4e5f60001"
	conn := openTestDB(t)
	if err := db.InsertPhoto(conn, db.Photo{Hash: fullHash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	w := &Watcher{
		GitPath:          git,
		OriginalsPath:    originals,
		PmsetPath:        writeFakeBin(t, t.TempDir(), "pmset", `echo "AC Power"`),
		PgrepPath:        writeFakeBin(t, t.TempDir(), "pgrep", `exit 1`),
		DB:               conn,
		Logger:           openTestLogger(t),
		DebounceDuration: 100 * time.Millisecond,
	}
	go w.Run()
	time.Sleep(200 * time.Millisecond)

	// File name embeds the first 8 chars of fullHash.
	xmpPath := filepath.Join(originals, "20260622_140311_a1b2c3d4.xmp")
	if err := os.WriteFile(xmpPath, []byte("xmp data"), 0o644); err != nil {
		t.Fatalf("write xmp: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	w.Stop()

	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", fullHash).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != db.StatusEdited {
		t.Errorf("status = %q, want %q", status, db.StatusEdited)
	}
}

// TestStatusUpdate_NoHashNoChange asserts that a file without an embedded hash8
// does not trigger any DB status change and does not cause an error.
func TestStatusUpdate_NoHashNoChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)
	originals := t.TempDir()
	setupRepo(t, git, originals)

	conn := openTestDB(t)
	const sentinelHash = "sentinelsentinel"
	if err := db.InsertPhoto(conn, db.Photo{Hash: sentinelHash}); err != nil {
		t.Fatalf("InsertPhoto: %v", err)
	}

	w := &Watcher{
		GitPath:          git,
		OriginalsPath:    originals,
		PmsetPath:        writeFakeBin(t, t.TempDir(), "pmset", `echo "AC Power"`),
		PgrepPath:        writeFakeBin(t, t.TempDir(), "pgrep", `exit 1`),
		DB:               conn,
		Logger:           openTestLogger(t),
		DebounceDuration: 100 * time.Millisecond,
	}
	go w.Run()
	time.Sleep(200 * time.Millisecond)

	// Filename has no _XXXXXXXX segment.
	noHash := filepath.Join(originals, "lightroom-export.xmp")
	if err := os.WriteFile(noHash, []byte("no hash here"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	w.Stop()

	var status string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", sentinelHash).Scan(&status); err != nil {
		t.Fatalf("query sentinel: %v", err)
	}
	if status != db.StatusRaw {
		t.Errorf("sentinel status = %q after no-hash file, want %q", status, db.StatusRaw)
	}
}

// ---- Push gating ------------------------------------------------------------

// TestPushGating verifies the dual gate: committed changes reach the bare remote
// only when both AC power is true AND Lightroom is not running.
func TestPushGating(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)

	run := func(t *testing.T, name, pgrepBody string, wantPushed bool) {
		t.Run(name, func(t *testing.T) {
			originals := t.TempDir()
			bare := t.TempDir()
			setupRepoWithRemote(t, git, originals, bare)

			binDir := t.TempDir()
			pmset := writeFakeBin(t, binDir, "pmset", `echo "Now drawing from 'AC Power'"`)
			pgrep := writeFakeBin(t, binDir, "pgrep", pgrepBody)

			w := &Watcher{
				GitPath:          git,
				OriginalsPath:    originals,
				PmsetPath:        pmset,
				PgrepPath:        pgrep,
				DB:               openTestDB(t),
				Logger:           openTestLogger(t),
				DebounceDuration: 100 * time.Millisecond,
			}
			go w.Run()
			time.Sleep(200 * time.Millisecond)

			// Record bare HEAD before the edit.
			bareHead := strings.TrimSpace(gitCmd(t, git, bare, "log", "-1", "--format=%H"))

			// Write a watched file to trigger a commit.
			xmpPath := filepath.Join(originals, "20260622_aabbccdd.xmp")
			if err := os.WriteFile(xmpPath, []byte("edit"), 0o644); err != nil {
				t.Fatalf("write xmp: %v", err)
			}
			time.Sleep(800 * time.Millisecond)
			w.Stop()

			// Local commit must always exist.
			localSubject := gitCmd(t, git, originals, "log", "-1", "--format=%s")
			if !strings.Contains(localSubject, "edit:") {
				t.Errorf("local HEAD = %q, want contains \"edit:\"", strings.TrimSpace(localSubject))
			}

			newBareHead := strings.TrimSpace(gitCmd(t, git, bare, "log", "-1", "--format=%H"))
			pushed := newBareHead != bareHead

			if pushed != wantPushed {
				t.Errorf("pushed = %v, want %v (bare HEAD: before=%s, after=%s)",
					pushed, wantPushed, bareHead[:8], newBareHead[:8])
			}
		})
	}

	run(t, "AC=true,LR=false_pushes", `exit 1`, true)
	run(t, "AC=true,LR=running_nopush", `exit 0`, false)
}

// ---- Dynamic subdirectory ---------------------------------------------------

// TestDynamicSubdirectory proves that a directory created AFTER Run() starts is
// automatically added to the watch list, so changes inside it are still detected.
func TestDynamicSubdirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)
	originals := t.TempDir()
	setupRepo(t, git, originals)

	w, _, _ := newWatcher(t, git, originals, 100*time.Millisecond)
	go w.Run()
	time.Sleep(200 * time.Millisecond) // let startup walk complete

	// Create YYYY/MM subdirectory structure AFTER the watcher has started.
	subdir := filepath.Join(originals, "2026", "06")
	if err := os.Mkdir(filepath.Join(originals, "2026"), 0o755); err != nil {
		t.Fatalf("mkdir 2026: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let watcher Add() 2026/

	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir 2026/06: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let watcher Add() 2026/06/

	// Write a .xmp into the newly-watched subdirectory.
	xmpPath := filepath.Join(subdir, "20260601_a1b2c3d4.xmp")
	if err := os.WriteFile(xmpPath, []byte("xmp"), 0o644); err != nil {
		t.Fatalf("write xmp: %v", err)
	}

	// Wait for debounce + commit.
	time.Sleep(800 * time.Millisecond)
	w.Stop()

	if n := gitLogCount(t, git, originals); n != 2 {
		t.Errorf("git log count = %d, want 2 (initial + 1 edit from dynamic subdir)", n)
	}
}

// Package core_test holds the cross-package concurrency integration test.
// It is deliberately the one place in the repo that imports ingest, outgest,
// sdcard, xmpwatcher, outgestwatcher, db, logging, and gitops together so
// that their shared state (one catalog.db, one originals/ git repo, one
// framelog.log) is exercised under genuinely concurrent real watchers rather
// than synthetic goroutines calling the same *Logger or *Pipeline directly.
package core_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
	"github.com/thevedantmodi/framelog/core/outgestwatcher"
	"github.com/thevedantmodi/framelog/core/sdcard"
	"github.com/thevedantmodi/framelog/core/xmpwatcher"
)

// ---- helpers ----------------------------------------------------------------

func itFindGit(t *testing.T) string {
	t.Helper()
	g, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	return g
}

func itWriteFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

func itInitRepo(t *testing.T, git, dir string) {
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

// itSetupRepoWithRemote creates a git repo in repoDir with an initial commit
// and a bare remote in bareDir, mirroring the FL-106 pattern used in
// ingest_test.go and xmpwatcher_test.go.
func itSetupRepoWithRemote(t *testing.T, git, repoDir, bareDir string) {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	itInitRepo(t, git, repoDir)
	run(bareDir, "init", "--bare")
	if err := os.WriteFile(filepath.Join(repoDir, ".gitkeep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(repoDir, "add", "-A")
	run(repoDir, "commit", "-m", "init")
	run(repoDir, "remote", "add", "origin", bareDir)
	run(repoDir, "push", "-u", "origin", "HEAD")
}

// itGitLogMessages returns all commit subjects in the given repo, newest first.
func itGitLogMessages(t *testing.T, git, dir string) []string {
	t.Helper()
	out, err := exec.Command(git, "-C", dir, "log", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			msgs = append(msgs, s)
		}
	}
	return msgs
}

// itReadLogLines returns all non-empty lines from a log file.
func itReadLogLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log %s: %v", path, err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// ---- TestConcurrent ---------------------------------------------------------

// TestConcurrent is the concurrent integration test mandated by the Phase 2
// hardening spec. It starts all three watchers (sdcard.Watcher,
// xmpwatcher.Watcher, outgestwatcher.Watcher) against a shared catalog.db
// and originals/ git repo, then simultaneously triggers an SD card mount, an
// XMP sidecar write, and a processed-file drop.
//
// The assertions confirm:
//   - No goroutine panicked (panics in time.AfterFunc callbacks or watcher
//     goroutines would otherwise be silent in a test run).
//   - catalog.db has exactly 3 rows with the right statuses — no missing rows
//     from a lost event, no duplicate rows from a double-fire.
//   - Every git commit message is parseable and matches a known format — no
//     interleaved/garbled commit message from two commits racing on the same
//     git commit invocation.
//   - Every line in framelog.log is parseable per PROTOCOL.md §5 — no torn
//     lines from two loggers writing at once (this exercises FL-206's mutex
//     under genuinely concurrent real watchers for the first time).
//
// Run five times to smoke out races that don't appear on the first attempt:
//
//	go test ./... -race -run TestConcurrent -count=5
func TestConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent integration test: requires real git and fsnotify")
	}

	git := itFindGit(t)

	// ---- shared directories -------------------------------------------------
	logPath := filepath.Join(t.TempDir(), "framelog.log")
	inbox := t.TempDir()
	originals := t.TempDir()
	processed := t.TempDir()
	bare := t.TempDir()
	volumes := t.TempDir()
	binDir := t.TempDir()

	itSetupRepoWithRemote(t, git, originals, bare)

	// ---- fake binaries ------------------------------------------------------
	// exiftool: fixed capture date + camera model; no real exiftool needed.
	exiftool := itWriteFakeBin(t, binDir, "exiftool",
		`echo '[{"Model":"X-T5","DateTimeOriginal":"2026:06:22 14:03:11"}]'`)
	// pmset: battery power → push skipped for both ingest and xmpwatcher.
	pmset := itWriteFakeBin(t, binDir, "pmset",
		`echo "Now drawing from 'Battery Power'"`)
	// pgrep: exit 1 (no process matched) → IsLightroomRunning returns false.
	pgrep := itWriteFakeBin(t, binDir, "pgrep", `exit 1`)
	// diskutil: reports Removable for any volume path, satisfying IsRemovableMedia.
	diskutil := itWriteFakeBin(t, binDir, "diskutil",
		`echo "   Removable Media:          Removable"`)

	// ---- shared DB and logger -----------------------------------------------
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	conn, err := db.Open(dbPath, false)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.InitDB(conn); err != nil {
		t.Fatalf("db.InitDB: %v", err)
	}

	logger, err := logging.New(logPath)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	// ---- pre-insert two rows with known hash8 prefixes ----------------------
	// Row A: xmpwatcher will update to "edited" when we write an XMP with
	// "aabbccdd" embedded in its filename.
	const hashXMP = "aabbccdddeadbeef"
	if err := db.InsertPhoto(conn, db.Photo{Hash: hashXMP}); err != nil {
		t.Fatalf("InsertPhoto xmp: %v", err)
	}
	// Row B: outgest.OrganizeFile will update to "published" when we drop a
	// processed file with "11223344" embedded in its filename.
	const hashOut = "11223344cafebabe"
	if err := db.InsertPhoto(conn, db.Photo{Hash: hashOut}); err != nil {
		t.Fatalf("InsertPhoto out: %v", err)
	}

	// ---- pipelines ----------------------------------------------------------
	ingestPipeline := &ingest.Pipeline{
		DB:            conn,
		Logger:        logger,
		InboxPath:     inbox,
		OriginalsPath: originals,
		ExiftoolPath:  exiftool,
		GitPath:       git,
		PmsetPath:     pmset,
		// RclonePath/BackupPath intentionally empty: backup disabled.
	}

	outgestPipeline := &outgest.Pipeline{
		DB:            conn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  exiftool,
	}

	// ---- watchers -----------------------------------------------------------
	sdW := &sdcard.Watcher{
		DiskutilPath: diskutil,
		VolumesRoot:  volumes,
		InboxPath:    inbox,
		PollInterval: 100 * time.Millisecond,
		Runner:       ingestPipeline,
		Logger:       logger,
	}

	xmpW := &xmpwatcher.Watcher{
		GitPath:          git,
		OriginalsPath:    originals,
		PmsetPath:        pmset,
		PgrepPath:        pgrep,
		DB:               conn,
		Logger:           logger,
		DebounceDuration: 50 * time.Millisecond,
	}

	outgestW := &outgestwatcher.Watcher{
		ProcessedPath:    processed,
		Outgest:          outgestPipeline,
		Logger:           logger,
		DebounceDuration: 50 * time.Millisecond,
	}

	// ---- start watchers with panic recovery ---------------------------------
	// A panic inside a goroutine started by time.AfterFunc or watcher.Run
	// would crash the process without reaching t.Errorf. Wrap each goroutine
	// with a recover that stores the panic so we can assert on it after the
	// wait period.
	var panicVal atomic.Value

	sdStop := make(chan struct{})
	var sdDone sync.WaitGroup

	launchWatcher := func(name string, fn func() error) {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					panicVal.Store(fmt.Sprintf("watcher %s panicked: %v", name, r))
				}
			}()
			if err := fn(); err != nil {
				t.Logf("watcher %s Run() returned: %v", name, err)
			}
		}()
	}

	sdDone.Add(1)
	go func() {
		defer sdDone.Done()
		defer func() {
			if r := recover(); r != nil {
				panicVal.Store(fmt.Sprintf("watcher sdcard panicked: %v", r))
			}
		}()
		sdW.Run(sdStop) //nolint:errcheck
	}()
	launchWatcher("xmpwatcher", xmpW.Run)
	launchWatcher("outgestwatcher", outgestW.Run)

	// Polling watcher needs no setup delay.
	time.Sleep(50 * time.Millisecond)

	// ---- fire all three triggers simultaneously ------------------------------
	// Use a closed channel to unblock all three goroutines at the same
	// instant — the closest approximation to simultaneous that a test can arrange.
	release := make(chan struct{})
	var triggered sync.WaitGroup
	triggered.Add(3)

	// Trigger 1: fake SD card mount under volumes/.
	// sdcard.Watcher sees a Create event for EOS_DIGITAL, sleeps 2 s, checks
	// IsRemovableMedia+HasDCIM, copies DCIM files to inbox, calls RunIngest.
	go func() {
		defer triggered.Done()
		<-release
		dcim := filepath.Join(volumes, "EOS_DIGITAL", "DCIM", "100CANON")
		if err := os.MkdirAll(dcim, 0o755); err != nil {
			t.Errorf("mkdir SD card DCIM: %v", err)
			return
		}
		if err := os.WriteFile(
			filepath.Join(dcim, "IMG_0001.RAF"),
			[]byte("fake raw bytes for ingest"),
			0o644,
		); err != nil {
			t.Errorf("write SD card photo: %v", err)
		}
	}()

	// Trigger 2: XMP sidecar write to originals/ (simulates a Lightroom edit
	// landing on a photo that was previously imported). The filename embeds
	// hash8 "aabbccdd" so xmpwatcher.runCommit updates hashXMP row to "edited".
	go func() {
		defer triggered.Done()
		<-release
		if err := os.WriteFile(
			filepath.Join(originals, "20260622_140311_aabbccdd.xmp"),
			[]byte(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>`+
				`<x:xmpmeta xmlns:x="adobe:ns:meta/"/>`+
				`<?xpacket end="w"?>`),
			0o644,
		); err != nil {
			t.Errorf("write XMP file: %v", err)
		}
	}()

	// Trigger 3: Lightroom export drop into processed/. The filename embeds
	// hash8 "11223344" so outgest.OrganizeFile updates hashOut row to "published".
	go func() {
		defer triggered.Done()
		<-release
		if err := os.WriteFile(
			filepath.Join(processed, "20260622_140311_11223344.jpg"),
			[]byte("fake processed export bytes"),
			0o644,
		); err != nil {
			t.Errorf("write processed file: %v", err)
		}
	}()

	close(release)    // unblock all three goroutines simultaneously
	triggered.Wait() // wait for all three writes to complete before sleeping

	// Wait past all debounce windows and the sdcard poll interval.
	// Timeline:
	//   ~50ms  — xmpwatcher debounce fires → git commit + DB update
	//   ~50ms  — outgestwatcher debounce fires → RunOutgest → DB update
	//   ~100ms — sdcard poll tick fires → CopyDCIM + RunIngest → git commit + DB insert
	//   + buffer
	time.Sleep(3 * time.Second)

	// Stop all watchers before asserting so no further events race with reads.
	close(sdStop)
	xmpW.Stop()
	outgestW.Stop()
	// sdDone.Wait() blocks until the sdcard goroutine fully exits, which means
	// any in-flight tick() — including RunIngest and its DB commit — has completed.
	// The 100ms sleep only covers xmp/outgest watcher goroutines.
	sdDone.Wait()
	time.Sleep(100 * time.Millisecond)

	// ---- assertions ---------------------------------------------------------

	// 1. No goroutine panicked.
	if v := panicVal.Load(); v != nil {
		t.Fatalf("goroutine panic detected: %s", v.(string))
	}

	// 2. DB row count: 2 pre-inserted + 1 from SD card ingest = 3.
	count, err := db.PhotoCount(conn)
	if err != nil {
		t.Fatalf("PhotoCount: %v", err)
	}
	if count != 3 {
		t.Errorf("photo count = %d, want 3 (2 pre-inserted + 1 from SD card ingest)", count)
	}

	// 3. XMP row → "edited" (xmpwatcher.runCommit updated it).
	var xmpStatus string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", hashXMP).Scan(&xmpStatus); err != nil {
		t.Fatalf("query xmp status: %v", err)
	}
	if xmpStatus != db.StatusEdited {
		t.Errorf("xmp row status = %q, want %q", xmpStatus, db.StatusEdited)
	}

	// 4. Outgest row → "published" (outgest.OrganizeFile updated it).
	var outStatus string
	if err := conn.QueryRow("SELECT status FROM photos WHERE hash = ?", hashOut).Scan(&outStatus); err != nil {
		t.Fatalf("query outgest status: %v", err)
	}
	if outStatus != db.StatusPublished {
		t.Errorf("outgest row status = %q, want %q", outStatus, db.StatusPublished)
	}

	// 5. SD card ingest row exists, appears exactly once.
	// Status is NOT asserted here: the xmpwatcher legitimately fires for the
	// XMP sidecar written by ingest and promotes the row to "edited" before
	// this check runs, so requiring "raw" would be a race against the watcher.
	var sdCount int
	if err := conn.QueryRow(
		"SELECT COUNT(*) FROM photos WHERE hash NOT IN (?, ?)",
		hashXMP, hashOut,
	).Scan(&sdCount); err != nil {
		t.Fatalf("query sd card count: %v", err)
	}
	if sdCount != 1 {
		t.Errorf("SD card import row count = %d, want exactly 1 (no missing row, no double-fire duplicate)", sdCount)
	}

	// 6. Git commit messages are all parseable and match a known format —
	// no interleaved lines from two concurrent `git commit` invocations.
	gitMsgs := itGitLogMessages(t, git, originals)
	if len(gitMsgs) < 2 {
		t.Errorf("git log has %d commit(s), want ≥2 (init + at least one pipeline commit)", len(gitMsgs))
	}
	knownMsgRe := regexp.MustCompile(
		`^(init|initial|ingest: \d{4}-\d{2}-\d{2} \(\d+ photos?\)|edit: \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \(\d+ files?\))$`,
	)
	for _, msg := range gitMsgs {
		if !knownMsgRe.MatchString(msg) {
			t.Errorf("garbled or unexpected git commit message: %q", msg)
		}
	}

	// 7. Every log line is parseable per PROTOCOL.md §5:
	//    "YYYY-MM-DD HH:MM:SS [PREFIX] message"
	// A torn line (from two loggers writing at once without FL-206's mutex)
	// would either be shorter than the prefix, contain a misplaced newline, or
	// not match this format at all.
	logLines := itReadLogLines(t, logPath)
	if len(logLines) == 0 {
		t.Error("framelog.log is empty — no pipeline activity was logged")
	}
	logLineRe := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[(INGEST|OUTGEST|XMP|BACKUP|GIT|CORE)\] .+$`,
	)
	for _, line := range logLines {
		if !logLineRe.MatchString(line) {
			t.Errorf("log line not parseable per PROTOCOL.md §5: %q", line)
		}
	}
}

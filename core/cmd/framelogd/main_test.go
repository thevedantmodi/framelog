package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/ipc"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
	"github.com/thevedantmodi/framelog/core/outgestwatcher"
	"github.com/thevedantmodi/framelog/core/triggerwatcher"
	"github.com/thevedantmodi/framelog/core/xmpwatcher"
)

// shortTempDir returns a temp directory with a short enough path to fit inside
// macOS's 104-byte Unix domain socket path limit when we append a filename.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fld*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fakeScript writes a shell script that exits 0 and (for exiftool) emits
// minimal valid JSON so ingest doesn't panic if it's ever accidentally called.
func fakeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("fakeScript %s: %v", name, err)
	}
	return p
}

// TestColdStart_DirectoriesAndDB proves that the daemon initialises its entire
// workspace from literally nothing — no inbox/, originals/, processed/ or
// catalog.db pre-created, no git repo. It calls initWorkspace (the same
// function mainRun invokes on each startup) and then starts run() to confirm
// the fully-wired daemon operates correctly in that cold-start state.
//
// This is a distinct claim from TestSmokeDaemon_StatusResponse (which
// pre-creates all directories before calling run) — "starts from literally
// nothing" needs its own name.
func TestColdStart_DirectoriesAndDB(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; skipping cold-start test")
	}

	base := shortTempDir(t) // completely empty — no subdirs, no files

	inbox := filepath.Join(base, "inbox")
	originals := filepath.Join(base, "originals")
	processed := filepath.Join(base, "processed")
	dbPath := filepath.Join(base, "catalog.db")

	// Pre-condition: none of the directories exist yet.
	for _, dir := range []string{inbox, originals, processed} {
		if _, err := os.Stat(dir); err == nil {
			t.Fatalf("pre-condition violated: %s already exists", dir)
		}
	}

	// initWorkspace is what mainRun() calls on every startup.
	if err := initWorkspace(inbox, originals, processed, gitPath); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}

	// All three directories must now exist.
	for _, dir := range []string{inbox, originals, processed} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory not created: %s: %v", dir, err)
		}
	}

	// originals/ must be a valid git repository.
	if _, err := os.Stat(filepath.Join(originals, ".git")); err != nil {
		t.Errorf("originals/.git not found after initWorkspace: %v", err)
	}
	out, err := exec.Command(gitPath, "-C", originals, "rev-parse", "--git-dir").CombinedOutput()
	if err != nil {
		t.Errorf("git -C originals rev-parse --git-dir: %v (%s)", err, out)
	}

	// DB must be openable and the photos table creatable from a blank slate.
	dbConn, err := db.Open(dbPath, false)
	if err != nil {
		t.Fatalf("db.Open on fresh path: %v", err)
	}
	t.Cleanup(func() { dbConn.Close() })
	if err := db.InitDB(dbConn); err != nil {
		t.Fatalf("db.InitDB: %v", err)
	}
	if _, err := dbConn.Exec("SELECT 1 FROM photos LIMIT 0"); err != nil {
		t.Errorf("photos table not present after InitDB: %v", err)
	}

	// initWorkspace must be idempotent — calling it again on an already-initialized
	// workspace must not error or re-init the git repo.
	if err := initWorkspace(inbox, originals, processed, gitPath); err != nil {
		t.Errorf("initWorkspace idempotent call: %v", err)
	}

	// Finally, prove run() starts and stops cleanly from this cold-start state —
	// specifically that the xmpwatcher and outgestwatcher can attach to the freshly
	// created directories without panicking or erroring.
	scripts := shortTempDir(t)
	fakeExiftool := fakeScript(t, scripts, "exiftool", `echo '[]'`)
	fakePgrep := fakeScript(t, scripts, "pgrep", `exit 1`) // Lightroom not running

	logFile := filepath.Join(base, "test.log")
	logger, err := logging.New(logFile)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	ingestP := &ingest.Pipeline{
		DB: dbConn, Logger: logger,
		InboxPath: inbox, OriginalsPath: originals,
		ExiftoolPath: fakeExiftool, GitPath: gitPath,
	}
	outgestP := &outgest.Pipeline{
		DB: dbConn, Logger: logger,
		ProcessedPath: processed, ExiftoolPath: fakeExiftool,
	}
	socketPath := filepath.Join(base, "d.sock")
	cs := &configSetter{pipeline: ingestP, path: ""}
	sp := &statusProvider{
		ingestPipeline: ingestP, outgestPipeline: outgestP,
		dbConn: dbConn, configSetter: cs,
	}
	rc := &runConfig{
		dbConn: dbConn, logger: logger,
		ingestPipeline: ingestP, outgestPipeline: outgestP,
		ipcServer: &ipc.Server{
			SocketPath: socketPath, Ingest: ingestP, Outgest: outgestP,
			Status: sp, Logger: logger, ReadDeadline: 2 * time.Second,
		},
		triggerWatcher: &triggerwatcher.Watcher{
			IngestTriggerPath:  filepath.Join(base, ".ingest_trigger"),
			OutgestTriggerPath: filepath.Join(base, ".outgest_trigger"),
			PollInterval: 50 * time.Millisecond,
			Ingest: ingestP, Outgest: outgestP, Logger: logger,
		},
		xmpW: &xmpwatcher.Watcher{
			GitPath: gitPath, OriginalsPath: originals,
			PgrepPath: fakePgrep, DB: dbConn, Logger: logger,
			DebounceDuration: 50 * time.Millisecond,
		},
		outgestW: &outgestwatcher.Watcher{
			ProcessedPath: processed, Outgest: outgestP, Logger: logger,
			DebounceDuration: 50 * time.Millisecond,
		},
	}

	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- run(rc, stop) }()

	// Give watchers time to attach, then shut down cleanly.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	if err := <-errCh; err != nil {
		t.Errorf("run() from cold-start state: %v", err)
	}
}

// TestSmokeDaemon_StatusResponse starts a fully-wired daemon (real DB, real
// IPC socket, real triggerwatcher and fsnotify watchers) against temp
// directories and fake binaries, dials the Unix socket, sends a status
// command, and asserts a well-formed response.
//
// This test proves that triggerwatcher, ipc, and the main daemon wiring are
// actually connected end-to-end — not just unit-tested in isolation.
func TestSmokeDaemon_StatusResponse(t *testing.T) {
	base := shortTempDir(t)

	// Directories the watchers need.
	inbox := filepath.Join(base, "inbox")
	originals := filepath.Join(base, "originals")
	processed := filepath.Join(base, "processed")
	for _, d := range []string{inbox, originals, processed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Fake binaries — won't be called during status; just need valid paths.
	scripts := shortTempDir(t)
	fakeExiftool := fakeScript(t, scripts, "exiftool",
		`echo '[{"SourceFile":"dummy","DateTimeOriginal":"2026:06:01 12:00:00"}]'`)
	fakeGit := fakeScript(t, scripts, "git", `exit 0`)
	fakePgrep := fakeScript(t, scripts, "pgrep", `exit 1`) // Lightroom not running

	// Logger.
	logFile := filepath.Join(base, "test.log")
	logger, err := logging.New(logFile)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	// Real DB.
	dbPath := filepath.Join(base, "catalog.db")
	dbConn, err := db.Open(dbPath, false)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbConn.Close() })
	if err := db.InitDB(dbConn); err != nil {
		t.Fatalf("db.InitDB: %v", err)
	}

	// Real pipelines (fake binary paths won't be invoked for a status command).
	ingestP := &ingest.Pipeline{
		DB:            dbConn,
		Logger:        logger,
		InboxPath:     inbox,
		OriginalsPath: originals,
		ExiftoolPath:  fakeExiftool,
		GitPath:       fakeGit,
	}
	outgestP := &outgest.Pipeline{
		DB:            dbConn,
		Logger:        logger,
		ProcessedPath: processed,
		ExiftoolPath:  fakeExiftool,
	}

	// IPC server — socket in shortTempDir to avoid the 104-char limit.
	socketPath := filepath.Join(base, "d.sock")
	cs := &configSetter{pipeline: ingestP, path: ""}
	sp := &statusProvider{
		ingestPipeline:  ingestP,
		outgestPipeline: outgestP,
		dbConn:          dbConn,
		configSetter:    cs,
	}
	ipcSrv := &ipc.Server{
		SocketPath:   socketPath,
		Ingest:       ingestP,
		Outgest:      outgestP,
		Status:       sp,
		Logger:       logger,
		ReadDeadline: 3 * time.Second,
	}

	// Trigger watcher paths (directories already exist above).
	tw := &triggerwatcher.Watcher{
		IngestTriggerPath:  filepath.Join(base, ".ingest_trigger"),
		OutgestTriggerPath: filepath.Join(base, ".outgest_trigger"),
		PollInterval:       50 * time.Millisecond,
		Ingest:             ingestP,
		Outgest:            outgestP,
		Logger:             logger,
	}

	// XMP watcher (watches originals/).
	xmpW := &xmpwatcher.Watcher{
		GitPath:          fakeGit,
		OriginalsPath:    originals,
		PmsetPath:        "", // empty → AC check skipped
		PgrepPath:        fakePgrep,
		DB:               dbConn,
		Logger:           logger,
		DebounceDuration: 50 * time.Millisecond,
	}

	// Outgest watcher.
	outgestW := &outgestwatcher.Watcher{
		ProcessedPath:    processed,
		Outgest:          outgestP,
		Logger:           logger,
		DebounceDuration: 50 * time.Millisecond,
	}

	rc := &runConfig{
		dbConn:          dbConn,
		logger:          logger,
		ingestPipeline:  ingestP,
		outgestPipeline: outgestP,
		ipcServer:       ipcSrv,
		triggerWatcher:  tw,
		xmpW:            xmpW,
		outgestW:        outgestW,
		sdcardW:         nil, // optional; omit in unit smoke test
	}

	// Start daemon in background.
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- run(rc, stop) }()
	t.Cleanup(func() {
		close(stop)
		<-errCh
	})

	// Wait briefly for IPC server to be ready.
	var conn net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", socketPath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("could not connect to IPC socket within 2s: %v", err)
	}
	defer conn.Close()

	// Send status command.
	fmt.Fprintf(conn, `{"command":"status"}`+"\n")

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", line, err)
	}

	if m["protocol_version"] != float64(1) {
		t.Errorf("protocol_version = %v, want 1", m["protocol_version"])
	}
	if m["ok"] != true {
		t.Errorf("ok = %v, want true (full response: %s)", m["ok"], line)
	}
	if _, exists := m["ingest_running"]; !exists {
		t.Error("response missing ingest_running field")
	}
	if _, exists := m["photo_count"]; !exists {
		t.Error("response missing photo_count field")
	}
}

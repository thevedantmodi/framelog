package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// shortTempDir returns a temp directory with a short path that fits within
// macOS's 104-byte Unix socket path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ---- fakes ------------------------------------------------------------------

type fakeIngest struct {
	mu      sync.Mutex
	counts  ingest.Counts
	err     error
	blockCh chan struct{} // if non-nil, RunIngest blocks until closed
}

func (f *fakeIngest) RunIngest() (ingest.Counts, error) {
	f.mu.Lock()
	ch := f.blockCh
	f.mu.Unlock()
	if ch != nil {
		<-ch
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts, f.err
}

type fakeOutgest struct {
	mu     sync.Mutex
	counts outgest.Counts
	err    error
}

func (f *fakeOutgest) RunOutgest() (outgest.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts, f.err
}

type fakeStatus struct {
	ingestRunning      bool
	outgestRunning     bool
	photoCount         int
	lastImport         string
	backupDriveMounted bool
}

func (f *fakeStatus) IngestRunning() bool      { return f.ingestRunning }
func (f *fakeStatus) OutgestRunning() bool     { return f.outgestRunning }
func (f *fakeStatus) PhotoCount() (int, error) { return f.photoCount, nil }
func (f *fakeStatus) LastImport() (string, error) {
	return f.lastImport, nil
}
func (f *fakeStatus) BackupDriveMounted() bool { return f.backupDriveMounted }

type fakeConfig struct {
	mu          sync.Mutex
	backupPath  string
	err         error
}

func (f *fakeConfig) SetBackupPath(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.backupPath = path
	return nil
}

// ---- helpers ----------------------------------------------------------------

func openTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(filepath.Join(t.TempDir(), "test.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func startServer(t *testing.T, fi *fakeIngest, fo *fakeOutgest, fs *fakeStatus, fc ConfigSetter) *Server {
	t.Helper()
	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	s := &Server{
		SocketPath:   socketPath,
		Ingest:       fi,
		Outgest:      fo,
		Status:       fs,
		Config:       fc,
		Logger:       openTestLogger(t),
		ReadDeadline: 2 * time.Second,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// dialJSON sends an arbitrary JSON payload and returns the parsed response.
func dialJSON(t *testing.T, socketPath string, payload string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return m
}

// dial sends one JSON request line and returns the parsed response map.
func dial(t *testing.T, socketPath, cmd string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, `{"command":%q}`+"\n", cmd); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return m
}

// ---- tests ------------------------------------------------------------------

func TestIngestNow_SuccessResponse(t *testing.T) {
	fi := &fakeIngest{counts: ingest.Counts{Imported: 3, Skipped: 1, Failed: 0}}
	s := startServer(t, fi, &fakeOutgest{}, &fakeStatus{}, &fakeConfig{})

	m := dial(t, s.SocketPath, "ingest_now")

	if m["protocol_version"] != float64(1) {
		t.Errorf("protocol_version = %v, want 1", m["protocol_version"])
	}
	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["imported"] != float64(3) {
		t.Errorf("imported = %v, want 3", m["imported"])
	}
	if m["skipped"] != float64(1) {
		t.Errorf("skipped = %v, want 1", m["skipped"])
	}
	if m["failed"] != float64(0) {
		t.Errorf("failed = %v, want 0", m["failed"])
	}
}

func TestIngestNow_AlreadyRunning(t *testing.T) {
	fi := &fakeIngest{err: ingest.ErrIngestAlreadyRunning}
	s := startServer(t, fi, &fakeOutgest{}, &fakeStatus{}, &fakeConfig{})

	m := dial(t, s.SocketPath, "ingest_now")

	if m["ok"] != false {
		t.Errorf("ok = %v, want false", m["ok"])
	}
	if m["error"] != "ingest_already_running" {
		t.Errorf("error = %v, want ingest_already_running", m["error"])
	}
}

func TestOutgestNow_SuccessResponse(t *testing.T) {
	fo := &fakeOutgest{counts: outgest.Counts{Moved: 2, Skipped: 0, Failed: 0}}
	s := startServer(t, &fakeIngest{}, fo, &fakeStatus{}, &fakeConfig{})

	m := dial(t, s.SocketPath, "outgest_now")

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["moved"] != float64(2) {
		t.Errorf("moved = %v, want 2", m["moved"])
	}
}

func TestOutgestNow_AlreadyRunning(t *testing.T) {
	fo := &fakeOutgest{err: outgest.ErrOutgestAlreadyRunning}
	s := startServer(t, &fakeIngest{}, fo, &fakeStatus{}, &fakeConfig{})

	m := dial(t, s.SocketPath, "outgest_now")

	if m["error"] != "outgest_already_running" {
		t.Errorf("error = %v, want outgest_already_running", m["error"])
	}
}

func TestStatus_AllFields(t *testing.T) {
	fs := &fakeStatus{
		ingestRunning:      false,
		outgestRunning:     true,
		photoCount:         42,
		lastImport:         "2026-06-20T14:02:00Z",
		backupDriveMounted: true,
	}
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, fs, &fakeConfig{})

	m := dial(t, s.SocketPath, "status")

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["ingest_running"] != false {
		t.Errorf("ingest_running = %v, want false", m["ingest_running"])
	}
	if m["outgest_running"] != true {
		t.Errorf("outgest_running = %v, want true", m["outgest_running"])
	}
	if m["photo_count"] != float64(42) {
		t.Errorf("photo_count = %v, want 42", m["photo_count"])
	}
	if m["last_import"] != "2026-06-20T14:02:00Z" {
		t.Errorf("last_import = %v, want 2026-06-20T14:02:00Z", m["last_import"])
	}
	if m["backup_drive_mounted"] != true {
		t.Errorf("backup_drive_mounted = %v, want true", m["backup_drive_mounted"])
	}
}

func TestStatus_EmptyLastImport(t *testing.T) {
	fs := &fakeStatus{photoCount: 0, lastImport: ""}
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, fs, &fakeConfig{})

	m := dial(t, s.SocketPath, "status")

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["last_import"] != "" {
		t.Errorf("last_import = %v, want empty string", m["last_import"])
	}
}

func TestUnknownCommand(t *testing.T) {
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, &fakeConfig{})

	m := dial(t, s.SocketPath, "frobnicate")

	if m["ok"] != false {
		t.Errorf("ok = %v, want false", m["ok"])
	}
	if m["error"] != "unknown_command" {
		t.Errorf("error = %v, want unknown_command", m["error"])
	}
}

func TestBadJSON(t *testing.T) {
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, &fakeConfig{})

	conn, err := net.Dial("unix", s.SocketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "not json at all\n")

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var m map[string]any
	json.Unmarshal([]byte(line), &m)
	if m["error"] != "bad_request" {
		t.Errorf("error = %v, want bad_request", m["error"])
	}
}

// TestStatus_NotBlockedByIngestNow is the critical concurrency test from the
// spec: a client that calls "ingest_now" and blocks holds the ingest pipeline,
// but a second client calling "status" must still get a response within a
// short window — proving status never waits on the in-flight ingest.
func TestStatus_NotBlockedByIngestNow(t *testing.T) {
	blockCh := make(chan struct{})
	fi := &fakeIngest{blockCh: blockCh}
	s := startServer(t, fi, &fakeOutgest{}, &fakeStatus{photoCount: 7}, &fakeConfig{})

	// Fire ingest_now and keep it blocked.
	go func() {
		conn, _ := net.Dial("unix", s.SocketPath)
		if conn != nil {
			fmt.Fprintf(conn, `{"command":"ingest_now"}`+"\n")
			bufio.NewReader(conn).ReadString('\n')
			conn.Close()
		}
	}()

	// Give the ingest goroutine time to start and block.
	time.Sleep(30 * time.Millisecond)

	// Status must come back within 200ms despite the blocked ingest.
	done := make(chan map[string]any, 1)
	go func() { done <- dial(t, s.SocketPath, "status") }()

	select {
	case m := <-done:
		if m["photo_count"] != float64(7) {
			t.Errorf("photo_count = %v, want 7", m["photo_count"])
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("status blocked: did not respond within 200ms while ingest_now was in flight")
	}

	close(blockCh) // unblock the blocked ingest goroutine
}

// TestSilentClient_ServerClosed verifies a client that connects and writes
// nothing is dropped by the server within roughly ReadDeadline, not held open.
func TestSilentClient_ServerClosed(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	s := &Server{
		SocketPath:   socketPath,
		Ingest:       &fakeIngest{},
		Outgest:      &fakeOutgest{},
		Status:       &fakeStatus{},
		Logger:       openTestLogger(t),
		ReadDeadline: 100 * time.Millisecond, // short — must be set before Start
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	conn, err := net.Dial("unix", s.SocketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Try to read — should hit EOF when server closes the conn after deadline.
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Error("expected server to close conn after ReadDeadline, got data instead")
	}
}

func TestStop_RemovesSocketFile(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "ipc.sock")
	s := &Server{
		SocketPath:   socketPath,
		Ingest:       &fakeIngest{},
		Outgest:      &fakeOutgest{},
		Status:       &fakeStatus{},
		Logger:       openTestLogger(t),
		ReadDeadline: 2 * time.Second,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket not found after Start: %v", err)
	}
	s.Stop()
	if _, err := os.Stat(socketPath); err == nil {
		t.Error("socket file still exists after Stop")
	}
}

func TestStart_StaleSocketSucceeds(t *testing.T) {
	socketPath := filepath.Join(shortTempDir(t), "stale.sock")
	// Create a stale file at the socket path.
	os.WriteFile(socketPath, []byte("stale"), 0o600)

	s := &Server{
		SocketPath:   socketPath,
		Ingest:       &fakeIngest{},
		Outgest:      &fakeOutgest{},
		Status:       &fakeStatus{},
		Logger:       openTestLogger(t),
		ReadDeadline: 2 * time.Second,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start with stale socket: %v", err)
	}
	s.Stop()
}

func TestSetBackupPath_Success(t *testing.T) {
	fc := &fakeConfig{}
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, fc)

	m := dialJSON(t, s.SocketPath, `{"command":"set_backup_path","path":"/Volumes/MyDrive"}`)

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["protocol_version"] != float64(1) {
		t.Errorf("protocol_version = %v, want 1", m["protocol_version"])
	}
	fc.mu.Lock()
	got := fc.backupPath
	fc.mu.Unlock()
	if got != "/Volumes/MyDrive" {
		t.Errorf("backupPath = %q, want /Volumes/MyDrive", got)
	}
}

func TestSetBackupPath_Empty(t *testing.T) {
	fc := &fakeConfig{backupPath: "/Volumes/OldDrive"}
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, fc)

	m := dialJSON(t, s.SocketPath, `{"command":"set_backup_path","path":""}`)

	if m["ok"] != true {
		t.Errorf("ok = %v, want true (empty path disables backup)", m["ok"])
	}
	fc.mu.Lock()
	got := fc.backupPath
	fc.mu.Unlock()
	if got != "" {
		t.Errorf("backupPath = %q, want empty string", got)
	}
}

func TestSetBackupPath_SetterError(t *testing.T) {
	fc := &fakeConfig{err: fmt.Errorf("disk full")}
	s := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, fc)

	m := dialJSON(t, s.SocketPath, `{"command":"set_backup_path","path":"/Volumes/MyDrive"}`)

	if m["ok"] != false {
		t.Errorf("ok = %v, want false", m["ok"])
	}
	if m["error"] != "internal_error" {
		t.Errorf("error = %v, want internal_error", m["error"])
	}
}

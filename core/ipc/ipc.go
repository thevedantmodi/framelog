// Package ipc implements the v2 IPC Unix domain socket server (PROTOCOL.md §3).
// Transport: line-delimited JSON, one connection per request — the client dials,
// writes one JSON line + "\n", reads one JSON response line, then closes.
//
// Concurrency rule (PROTOCOL.md §3): "status must be served by a separate,
// always-available handler — don't let it share a lock with ingest_now/outgest_now."
// The status command never calls Runner methods; it reads only the quick
// mutex-guarded IngestRunning/OutgestRunning booleans and two DB queries via
// StatusProvider. This means a concurrent long-running ingest never makes the
// core *look* unreachable to a polling status client.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// ConfigSetter persists a new backup path and propagates it to the running
// pipelines. Implemented in cmd/framelogd/main.go next to the concrete
// StatusProvider, for the same cross-package-boundary reason.
type ConfigSetter interface {
	SetBackupPath(path string) error
}

// StatusProvider is the read-only view of pipeline state the status handler
// needs. The concrete implementation (in cmd/framelogd/main.go) wraps
// ingest.Pipeline, outgest.Pipeline, db, and backup — all live across package
// boundaries, so the wrapper lives in main rather than here.
type StatusProvider interface {
	IngestRunning() bool
	OutgestRunning() bool
	PhotoCount() (int, error)
	LastImport() (string, error)
	BackupDriveMounted() bool
	Paused() bool
}

// PauseController pauses/resumes automatic and on-demand ingest+outgest
// together — "pause framelog" is a single global toggle, not per-pipeline.
// The concrete implementation (in cmd/framelogd/main.go) calls Pause/Resume
// on both ingest.Pipeline and outgest.Pipeline.
type PauseController interface {
	Pause()
	Resume()
}

// Server accepts connections on a Unix domain socket and dispatches one
// line-delimited JSON command per connection.
type Server struct {
	SocketPath string
	Ingest     ingest.Runner
	Outgest    outgest.Runner
	Status     StatusProvider
	Config     ConfigSetter
	Pause      PauseController
	Logger     *logging.Logger
	// Version is stamped by the build system and reported in status responses so
	// the Swift app can detect when the bundled binary is newer than the running
	// daemon and trigger a silent re-install.
	Version string
	// ReadDeadline is a server-side mirror of the client's 2s dial timeout
	// (PROTOCOL.md §3). A client that connects and never writes must not hold
	// a goroutine open indefinitely. Default: 5s in production (set by main).
	ReadDeadline time.Duration

	ln net.Listener
}

// Start creates the socket directory, removes any stale socket file from a
// previous unclean shutdown, binds, chmods the socket to 0600, and launches
// the accept loop in a goroutine.
func (s *Server) Start() error {
	dir := filepath.Dir(s.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ipc: mkdir %s: %w", dir, err)
	}

	// Stale socket from previous unclean shutdown — must not block binding.
	if err := os.Remove(s.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("ipc: remove stale socket %s: %w", s.SocketPath, err)
	}

	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return fmt.Errorf("ipc: listen %s: %w", s.SocketPath, err)
	}

	// 0600: user-only access. PROTOCOL.md doesn't specify a mode; this is a
	// sensible default for a single-user local socket.
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("ipc: chmod socket: %w", err)
	}

	s.ln = ln
	go s.acceptLoop()
	return nil
}

// Stop closes the listener (causing acceptLoop to exit) and removes the socket
// file so it does not linger after the process exits.
func (s *Server) Stop() error {
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	os.Remove(s.SocketPath) //nolint:errcheck — best-effort cleanup
	return err
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed via Stop()
		}
		go s.handleConn(conn)
	}
}

// request is parsed from the client line. Path is only used by set_backup_path.
type request struct {
	Command string `json:"command"`
	Path    string `json:"path,omitempty"`
}

// Per-command response structs — separate types so each command gets exactly
// the fields PROTOCOL.md §3 specifies, with no "omitempty" tricks hiding zeros.

type errResp struct {
	ProtocolVersion int    `json:"protocol_version"`
	OK              bool   `json:"ok"`
	Error           string `json:"error"`
}

type ingestOKResp struct {
	ProtocolVersion int `json:"protocol_version"`
	OK              bool `json:"ok"`
	Imported        int `json:"imported"`
	Skipped         int `json:"skipped"`
	Failed          int `json:"failed"`
}

type outgestOKResp struct {
	ProtocolVersion int `json:"protocol_version"`
	OK              bool `json:"ok"`
	Moved           int `json:"moved"`
	Skipped         int `json:"skipped"`
	Failed          int `json:"failed"`
}

type setBackupPathOKResp struct {
	ProtocolVersion int  `json:"protocol_version"`
	OK              bool `json:"ok"`
}

type pauseOKResp struct {
	ProtocolVersion int  `json:"protocol_version"`
	OK              bool `json:"ok"`
	Paused          bool `json:"paused"`
}

type statusResp struct {
	ProtocolVersion    int    `json:"protocol_version"`
	OK                 bool   `json:"ok"`
	IngestRunning      bool   `json:"ingest_running"`
	OutgestRunning     bool   `json:"outgest_running"`
	PhotoCount         int    `json:"photo_count"`
	LastImport         string `json:"last_import"`
	BackupDriveMounted bool   `json:"backup_drive_mounted"`
	DaemonVersion      string `json:"daemon_version"`
	Paused             bool   `json:"paused"`
}

// deadline returns the configured ReadDeadline, defaulting to 5s.
func (s *Server) deadline() time.Duration {
	if s.ReadDeadline > 0 {
		return s.ReadDeadline
	}
	return 5 * time.Second
}

// writeResp writes one JSON response line. The write deadline is set fresh
// here, NOT inherited from the request-read deadline: ingest_now/outgest_now
// run their pipeline synchronously and can take far longer than the read
// window, so a deadline stamped before the run would already be expired by
// the time the response is written — the client would see the connection
// close with no reply and a successful run would look like a failure.
func (s *Server) writeResp(conn net.Conn, v any) {
	conn.SetWriteDeadline(time.Now().Add(s.deadline())) //nolint:errcheck
	b, _ := json.Marshal(v)
	b = append(b, '\n')
	conn.Write(b) //nolint:errcheck — nothing useful to do if the write fails
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Read deadline only — the write deadline is set per-response in writeResp
	// (see comment there). A silent client is still dropped after this window.
	conn.SetReadDeadline(time.Now().Add(s.deadline())) //nolint:errcheck

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return // read timeout or closed — no response per spec
	}

	var req request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "bad_request"})
		return
	}

	switch req.Command {
	case "ingest_now":
		counts, err := s.Ingest.RunIngest()
		if err != nil {
			if errors.Is(err, ingest.ErrIngestAlreadyRunning) {
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "ingest_already_running"})
			} else if errors.Is(err, ingest.ErrIngestPaused) {
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "ingest_paused"})
			} else {
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc ingest_now error: %v", err))
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			}
			return
		}
		s.writeResp(conn, ingestOKResp{
			ProtocolVersion: 1, OK: true,
			Imported: counts.Imported, Skipped: counts.Skipped, Failed: counts.Failed,
		})

	case "outgest_now":
		counts, err := s.Outgest.RunOutgest()
		if err != nil {
			if errors.Is(err, outgest.ErrOutgestAlreadyRunning) {
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "outgest_already_running"})
			} else if errors.Is(err, outgest.ErrOutgestPaused) {
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "outgest_paused"})
			} else {
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc outgest_now error: %v", err))
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			}
			return
		}
		s.writeResp(conn, outgestOKResp{
			ProtocolVersion: 1, OK: true,
			Moved: counts.Moved, Skipped: counts.Skipped, Failed: counts.Failed,
		})

	case "status":
		// This case must never call RunIngest/RunOutgest — only the quick
		// mutex-guarded IngestRunning/OutgestRunning reads and two DB queries.
		photoCount, err := s.Status.PhotoCount()
		if err != nil {
			s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc status PhotoCount: %v", err))
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		lastImport, err := s.Status.LastImport()
		if err != nil {
			s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc status LastImport: %v", err))
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		s.writeResp(conn, statusResp{
			ProtocolVersion:    1,
			OK:                 true,
			IngestRunning:      s.Status.IngestRunning(),
			OutgestRunning:     s.Status.OutgestRunning(),
			PhotoCount:         photoCount,
			LastImport:         lastImport,
			BackupDriveMounted: s.Status.BackupDriveMounted(),
			DaemonVersion:      s.Version,
			Paused:             s.Status.Paused(),
		})

	case "pause":
		if s.Pause == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		s.Pause.Pause()
		s.writeResp(conn, pauseOKResp{ProtocolVersion: 1, OK: true, Paused: true})

	case "resume":
		if s.Pause == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		s.Pause.Resume()
		s.writeResp(conn, pauseOKResp{ProtocolVersion: 1, OK: true, Paused: false})

	case "set_backup_path":
		if s.Config == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		if err := s.Config.SetBackupPath(req.Path); err != nil {
			s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc set_backup_path error: %v", err))
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		s.writeResp(conn, setBackupPathOKResp{ProtocolVersion: 1, OK: true})

	default:
		s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "unknown_command"})
	}
}

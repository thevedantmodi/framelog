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

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/cardverify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
)

// ConfigSetter persists a new backup path and propagates it to the running
// pipelines. Implemented in cmd/framelogd/main.go next to the concrete
// StatusProvider, for the same cross-package-boundary reason.
type ConfigSetter interface {
	SetBackupPath(path string) error
	// SetImportTypes persists and applies the import file types; nil
	// restores the defaults. Returns an error wrapping
	// config.ErrInvalidFileType when the list is rejected.
	SetImportTypes(types []string) error
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
	// BackupConfigured distinguishes "no backup path set" from "backup drive
	// unplugged" — both report BackupDriveMounted()==false, but the frontend
	// shows different guidance for each (PROTOCOL.md §3).
	BackupConfigured() bool
	Paused() bool
}

// Capabilities reports which optional binaries were found at daemon startup.
// A false value means the corresponding feature is silently disabled — the
// frontend surfaces this in the menu instead of requiring a log tail
// (PROTOCOL.md §3). Static for the life of the process; set by main from the
// same Find* results it logs.
type Capabilities struct {
	SDCardWatch    bool `json:"sd_card_watch"`   // diskutil found
	Backup         bool `json:"backup"`          // rclone found
	ACPowerGate    bool `json:"ac_power_gate"`   // pmset found
	LightroomCheck bool `json:"lightroom_check"` // pgrep found
}

// PauseController pauses/resumes automatic and on-demand ingest+outgest
// together — "pause framelog" is a single global toggle, not per-pipeline.
// The concrete implementation (in cmd/framelogd/main.go) calls Pause/Resume
// on both ingest.Pipeline and outgest.Pipeline.
type PauseController interface {
	Pause()
	Resume()
}

// ReportProvider supplies the richer status fields: live activity, the
// backup outcome, and the last card verification. Every method must be fast
// (a mutex read or a single-row DB query) — it is called from the status
// handler, which must never block behind a running pipeline.
type ReportProvider interface {
	Activity() activity.Snapshot
	Backup() (BackupSummary, error)
	LastCard() (*db.CardReport, error)
	// CardPreview is the quick "what's new" result for the mounted card,
	// or nil when no card is mounted (or none has been scanned).
	CardPreview() *cardverify.Preview
}

// CardPreviewSummary is "card_preview" in the status response.
type CardPreviewSummary struct {
	VolumeName     string `json:"volume_name"`
	CheckedAt      string `json:"checked_at"`
	FilesTotal     int    `json:"files_total"`
	BytesTotal     int64  `json:"bytes_total"`
	FilesNew       int    `json:"files_new"`
	BytesNew       int64  `json:"bytes_new"`
	FilesInLibrary int    `json:"files_in_library"`
	FilesIgnored   int    `json:"files_ignored"`
	FilesUnhandled int    `json:"files_unhandled"`
	NewFileTypes   string `json:"new_file_types"`
}

// NewCardPreviewSummary converts a preview to its wire shape.
func NewCardPreviewSummary(p *cardverify.Preview) CardPreviewSummary {
	return CardPreviewSummary{
		VolumeName: p.VolumeName, CheckedAt: p.CheckedAt.UTC().Format(time.RFC3339),
		FilesTotal: p.Files, BytesTotal: p.Bytes, FilesNew: len(p.New), BytesNew: p.NewBytes,
		FilesInLibrary: p.InLibrary, FilesIgnored: p.Ignored, FilesUnhandled: p.Unhandled,
		NewFileTypes: p.NewTypes(),
	}
}

// CardVerifier re-verifies the mounted card on demand (sdcard.Watcher).
type CardVerifier interface {
	VerifyMounted() (db.CardReport, error)
}

// BackupRunner runs the library backup on demand (ingest.Pipeline).
type BackupRunner interface {
	RunBackup() (db.BackupRun, error)
}

// BackupSummary is the "backup" object in the status response.
type BackupSummary struct {
	// Pending is true when the library may not be fully on the configured
	// backup drive: the last attempt failed, the drive was missing, or no
	// backup has run since the destination was set. False when backup is
	// not configured.
	Pending       bool   `json:"pending"`
	LastOutcome   string `json:"last_outcome"` // ok|failed|drive_missing|rclone_missing, "" = never ran
	LastError     string `json:"last_error"`   // human-readable, "" on success
	LastRunAt     string `json:"last_run_at"`
	LastSuccessAt string `json:"last_success_at"`
	FilesCopied   int    `json:"files_copied"`
	FilesFailed   int    `json:"files_failed"`
}

// CardSummary is a card verification report on the wire ("last_card" in
// status, "card" in the verify_card response).
type CardSummary struct {
	ID            int64    `json:"id"`
	VolumeName    string   `json:"volume_name"`
	VolumeUUID    string   `json:"volume_uuid"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	SafeToFormat  bool     `json:"safe_to_format"`
	FilesTotal    int      `json:"files_total"`
	BytesTotal    int64    `json:"bytes_total"`
	FilesVerified int      `json:"files_verified"`
	FilesProblem  int      `json:"files_problem"`
	FilesIgnored  int      `json:"files_ignored"`
	FilesNew      int      `json:"files_new"`
	FilesExisting int      `json:"files_existing"`
	FileTypes     string   `json:"file_types"`
	CameraModels  string   `json:"camera_models"`
	CaptureFirst  string   `json:"capture_first"`
	CaptureLast   string   `json:"capture_last"`
	BackupState   string   `json:"backup_state"`
	Problems      []string `json:"problems"`
	ReportPath    string   `json:"report_path"`
}

// NewCardSummary converts a stored report to its wire shape.
func NewCardSummary(r db.CardReport) CardSummary {
	problems := r.Problems
	if problems == nil {
		problems = []string{} // always an array on the wire, never null
	}
	return CardSummary{
		ID: r.ID, VolumeName: r.VolumeName, VolumeUUID: r.VolumeUUID,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, SafeToFormat: r.SafeToFormat,
		FilesTotal: r.FilesTotal, BytesTotal: r.BytesTotal, FilesVerified: r.FilesVerified,
		FilesProblem: r.FilesProblem, FilesIgnored: r.FilesIgnored, FilesNew: r.FilesNew, FilesExisting: r.FilesExisting,
		FileTypes: r.FileTypes, CameraModels: r.CameraModels,
		CaptureFirst: r.CaptureFirst, CaptureLast: r.CaptureLast,
		BackupState: r.BackupState, Problems: problems, ReportPath: r.ReportPath,
	}
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
	// Reports, Card, and Backup are optional (nil disables the status
	// fields / commands they back — older wiring and tests keep working).
	Reports ReportProvider
	Card    CardVerifier
	Backup  BackupRunner
	Logger  *logging.Logger
	// Version is stamped by the build system and reported in status responses so
	// the Swift app can detect when the bundled binary is newer than the running
	// daemon and trigger a silent re-install.
	Version string
	// ReadDeadline is a server-side mirror of the client's 2s dial timeout
	// (PROTOCOL.md §3). A client that connects and never writes must not hold
	// a goroutine open indefinitely. Default: 5s in production (set by main).
	ReadDeadline time.Duration
	// Caps is set once at wiring time from binary resolution and echoed in
	// every status response.
	Caps Capabilities

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
	// Types is used by set_import_types. A pointer so a missing or null
	// "types" (reset to defaults) is distinguishable from [] (rejected).
	Types *[]string `json:"types,omitempty"`
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
	ProtocolVersion    int          `json:"protocol_version"`
	OK                 bool         `json:"ok"`
	IngestRunning      bool         `json:"ingest_running"`
	OutgestRunning     bool         `json:"outgest_running"`
	PhotoCount         int          `json:"photo_count"`
	LastImport         string       `json:"last_import"`
	BackupDriveMounted bool         `json:"backup_drive_mounted"`
	BackupConfigured   bool         `json:"backup_configured"`
	DaemonVersion      string       `json:"daemon_version"`
	Paused             bool         `json:"paused"`
	Capabilities       Capabilities `json:"capabilities"`
	// FileTypes lists every known camera type (on or off) and any custom
	// types, for the File Types menu.
	FileTypes []config.FileTypeSetting `json:"file_types"`
	// Present when the server has a ReportProvider. last_card is absent
	// until the first card has been verified.
	Activity *activity.Snapshot `json:"activity,omitempty"`
	Backup   *BackupSummary     `json:"backup,omitempty"`
	LastCard *CardSummary       `json:"last_card,omitempty"`
	// CardPreview is present while a scanned card is mounted.
	CardPreview *CardPreviewSummary `json:"card_preview,omitempty"`
}

type importTypesOKResp struct {
	ProtocolVersion int                      `json:"protocol_version"`
	OK              bool                     `json:"ok"`
	FileTypes       []config.FileTypeSetting `json:"file_types"`
}

type verifyCardOKResp struct {
	ProtocolVersion int         `json:"protocol_version"`
	OK              bool        `json:"ok"`
	Card            CardSummary `json:"card"`
}

type backupNowOKResp struct {
	ProtocolVersion int    `json:"protocol_version"`
	OK              bool   `json:"ok"`
	Outcome         string `json:"outcome"`
	FilesCopied     int    `json:"files_copied"`
	FilesFailed     int    `json:"files_failed"`
	Error           string `json:"backup_error"`
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
		resp := statusResp{
			ProtocolVersion:    1,
			OK:                 true,
			IngestRunning:      s.Status.IngestRunning(),
			OutgestRunning:     s.Status.OutgestRunning(),
			PhotoCount:         photoCount,
			LastImport:         lastImport,
			BackupDriveMounted: s.Status.BackupDriveMounted(),
			BackupConfigured:   s.Status.BackupConfigured(),
			DaemonVersion:      s.Version,
			Paused:             s.Status.Paused(),
			Capabilities:       s.Caps,
			FileTypes:          config.FileTypeSettings(),
		}
		if s.Reports != nil {
			// A failure reading report history degrades to "field absent"
			// rather than failing status: reachability matters more.
			act := s.Reports.Activity()
			resp.Activity = &act
			if b, err := s.Reports.Backup(); err != nil {
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc status Backup: %v", err))
			} else {
				resp.Backup = &b
			}
			if c, err := s.Reports.LastCard(); err != nil {
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc status LastCard: %v", err))
			} else if c != nil {
				cs := NewCardSummary(*c)
				resp.LastCard = &cs
			}
			if p := s.Reports.CardPreview(); p != nil {
				ps := NewCardPreviewSummary(p)
				resp.CardPreview = &ps
			}
		}
		s.writeResp(conn, resp)

	case "set_import_types":
		if s.Config == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		var types []string // nil = reset to defaults
		if req.Types != nil {
			types = *req.Types
			if types == nil {
				types = []string{} // explicit [] is a (rejected) empty set, not a reset
			}
		}
		if err := s.Config.SetImportTypes(types); err != nil {
			if errors.Is(err, config.ErrInvalidFileType) {
				s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "invalid_file_type"})
				return
			}
			s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc set_import_types error: %v", err))
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		s.writeResp(conn, importTypesOKResp{ProtocolVersion: 1, OK: true, FileTypes: config.FileTypeSettings()})

	case "verify_card":
		if s.Card == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "verify_unavailable"})
			return
		}
		rep, err := s.Card.VerifyMounted()
		if err != nil {
			code := err.Error()
			switch code {
			case "no_card", "card_busy", "verify_unavailable":
			default:
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc verify_card error: %v", err))
				code = "internal_error"
			}
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: code})
			return
		}
		s.writeResp(conn, verifyCardOKResp{ProtocolVersion: 1, OK: true, Card: NewCardSummary(rep)})

	case "backup_now":
		if s.Backup == nil {
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: "internal_error"})
			return
		}
		run, err := s.Backup.RunBackup()
		if err != nil {
			code := "internal_error"
			switch {
			case errors.Is(err, ingest.ErrBackupNotConfigured):
				code = "backup_not_configured"
			case errors.Is(err, ingest.ErrIngestAlreadyRunning):
				code = "ingest_already_running"
			case errors.Is(err, ingest.ErrIngestPaused):
				code = "ingest_paused"
			default:
				s.Logger.Log(logging.PrefixCore, fmt.Sprintf("ipc backup_now error: %v", err))
			}
			s.writeResp(conn, errResp{ProtocolVersion: 1, OK: false, Error: code})
			return
		}
		s.writeResp(conn, backupNowOKResp{ProtocolVersion: 1, OK: true, Outcome: run.Outcome,
			FilesCopied: run.FilesCopied, FilesFailed: run.FilesFailed, Error: run.Error})

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

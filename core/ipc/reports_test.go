package ipc

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/cardverify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
)

type fakeReports struct {
	preview   *cardverify.Preview
	act       activity.Snapshot
	backup    BackupSummary
	backupErr error
	card      *db.CardReport
	cardErr   error
}

func (f *fakeReports) Activity() activity.Snapshot       { return f.act }
func (f *fakeReports) Backup() (BackupSummary, error)    { return f.backup, f.backupErr }
func (f *fakeReports) LastCard() (*db.CardReport, error) { return f.card, f.cardErr }
func (f *fakeReports) CardPreview() *cardverify.Preview  { return f.preview }

type fakeCard struct {
	rep db.CardReport
	err error
}

func (f *fakeCard) VerifyMounted() (db.CardReport, error) { return f.rep, f.err }

type fakeBackup struct {
	run db.BackupRun
	err error
}

func (f *fakeBackup) RunBackup() (db.BackupRun, error) { return f.run, f.err }

func startReportServer(t *testing.T, r ReportProvider, c CardVerifier, b BackupRunner) *Server {
	t.Helper()
	s := &Server{
		SocketPath:   filepath.Join(shortTempDir(t), "ipc.sock"),
		Ingest:       &fakeIngest{},
		Outgest:      &fakeOutgest{},
		Status:       &fakeStatus{},
		Reports:      r,
		Card:         c,
		Backup:       b,
		Logger:       openTestLogger(t),
		ReadDeadline: 2 * time.Second,
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

func TestStatus_IncludesActivityBackupAndLastCard(t *testing.T) {
	fr := &fakeReports{
		act: activity.Snapshot{Phase: activity.PhaseVerifying, Volume: "Untitled",
			Done: 12, Total: 340, CurrentFile: "DSC00012.ARW"},
		backup: BackupSummary{Pending: true, LastOutcome: "failed",
			LastError: "Backup drive is full", FilesFailed: 3},
		card: &db.CardReport{ID: 7, VolumeName: "Untitled", SafeToFormat: true,
			FilesTotal: 340, FilesVerified: 340, FileTypes: "ARW 340", ReportPath: "/r.txt"},
	}
	s := startReportServer(t, fr, nil, nil)
	m := dial(t, s.SocketPath, "status")

	act, ok := m["activity"].(map[string]any)
	if !ok || act["phase"] != "verifying" || act["done"] != float64(12) ||
		act["total"] != float64(340) || act["current_file"] != "DSC00012.ARW" {
		t.Errorf("activity = %v", m["activity"])
	}
	b, ok := m["backup"].(map[string]any)
	if !ok || b["pending"] != true || b["last_error"] != "Backup drive is full" ||
		b["files_failed"] != float64(3) {
		t.Errorf("backup = %v", m["backup"])
	}
	c, ok := m["last_card"].(map[string]any)
	if !ok || c["safe_to_format"] != true || c["volume_name"] != "Untitled" ||
		c["report_path"] != "/r.txt" {
		t.Errorf("last_card = %v", m["last_card"])
	}
	if p, ok := c["problems"].([]any); !ok || len(p) != 0 {
		t.Errorf("problems should be an empty array, got %#v", c["problems"])
	}
}

func TestStatus_ReportFieldsAbsentWithoutProvider(t *testing.T) {
	s := startReportServer(t, nil, nil, nil)
	m := dial(t, s.SocketPath, "status")
	for _, k := range []string{"activity", "backup", "last_card"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s present without a ReportProvider", k)
		}
	}
}

func TestStatus_ReportErrorsDegradeNotFail(t *testing.T) {
	fr := &fakeReports{backupErr: errors.New("db locked"), cardErr: errors.New("db locked")}
	s := startReportServer(t, fr, nil, nil)
	m := dial(t, s.SocketPath, "status")
	if m["ok"] != true {
		t.Fatalf("status failed on report error: %v", m)
	}
	if _, ok := m["backup"]; ok {
		t.Error("backup present despite error")
	}
	if _, ok := m["activity"]; !ok {
		t.Error("activity should still be reported")
	}
}

func TestStatus_NoCardYetOmitsLastCard(t *testing.T) {
	s := startReportServer(t, &fakeReports{}, nil, nil)
	m := dial(t, s.SocketPath, "status")
	if _, ok := m["last_card"]; ok {
		t.Error("last_card present before any card was verified")
	}
}

func TestVerifyCard(t *testing.T) {
	fc := &fakeCard{rep: db.CardReport{ID: 3, VolumeName: "Untitled", SafeToFormat: false,
		Problems: []string{"1 file is not in the library yet"}}}
	s := startReportServer(t, nil, fc, nil)
	m := dial(t, s.SocketPath, "verify_card")
	card, ok := m["card"].(map[string]any)
	if m["ok"] != true || !ok || card["safe_to_format"] != false || card["id"] != float64(3) {
		t.Fatalf("verify_card = %v", m)
	}

	for _, code := range []string{"no_card", "card_busy", "verify_unavailable"} {
		fc.err = errors.New(code)
		if m := dial(t, s.SocketPath, "verify_card"); m["error"] != code {
			t.Errorf("err %s → %v", code, m)
		}
	}
	fc.err = errors.New("disk exploded")
	if m := dial(t, s.SocketPath, "verify_card"); m["error"] != "internal_error" {
		t.Errorf("unknown err → %v", m)
	}
}

func TestVerifyCard_Unavailable(t *testing.T) {
	s := startReportServer(t, nil, nil, nil)
	if m := dial(t, s.SocketPath, "verify_card"); m["error"] != "verify_unavailable" {
		t.Errorf("verify_card without verifier = %v", m)
	}
}

func TestBackupNow(t *testing.T) {
	fb := &fakeBackup{run: db.BackupRun{Outcome: "failed", FilesCopied: 2, FilesFailed: 1,
		Error: "Backup drive is full"}}
	s := startReportServer(t, nil, nil, fb)
	m := dial(t, s.SocketPath, "backup_now")
	if m["ok"] != true || m["outcome"] != "failed" || m["files_copied"] != float64(2) ||
		m["backup_error"] != "Backup drive is full" {
		t.Fatalf("backup_now = %v", m)
	}

	cases := map[error]string{
		ingest.ErrBackupNotConfigured:  "backup_not_configured",
		ingest.ErrIngestAlreadyRunning: "ingest_already_running",
		ingest.ErrIngestPaused:         "ingest_paused",
		errors.New("boom"):             "internal_error",
	}
	for err, code := range cases {
		fb.err = err
		if m := dial(t, s.SocketPath, "backup_now"); m["ok"] != false || m["error"] != code {
			t.Errorf("%v → %v", err, m)
		}
	}
}

func TestStatus_CardPreview(t *testing.T) {
	fr := &fakeReports{preview: &cardverify.Preview{
		VolumeName: "Untitled", CheckedAt: time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC),
		Files: 5, Bytes: 500, InLibrary: 3, NewBytes: 200, Ignored: 1,
		New: []cardverify.CardFile{{RelPath: "DCIM/100MSDCF/A.ARW", Size: 100},
			{RelPath: "DCIM/100MSDCF/A.JPG", Size: 100}},
	}}
	s := startReportServer(t, fr, nil, nil)
	m := dial(t, s.SocketPath, "status")
	p, ok := m["card_preview"].(map[string]any)
	if !ok {
		t.Fatalf("card_preview missing: %v", m)
	}
	if p["files_new"] != float64(2) || p["files_in_library"] != float64(3) ||
		p["files_ignored"] != float64(1) || p["new_file_types"] != "ARW 1 · JPG 1" ||
		p["checked_at"] != "2026-11-07T18:00:00Z" {
		t.Errorf("card_preview = %v", p)
	}

	fr.preview = nil
	if _, ok := dial(t, s.SocketPath, "status")["card_preview"]; ok {
		t.Error("card_preview present with no card")
	}
}

func TestStatus_FileTypes(t *testing.T) {
	s := startReportServer(t, nil, nil, nil)
	m := dial(t, s.SocketPath, "status")
	rows, ok := m["file_types"].([]any)
	if !ok || len(rows) != len(config.KnownMediaTypes) {
		t.Fatalf("file_types = %v", m["file_types"])
	}
	first := rows[0].(map[string]any)
	if first["ext"] != ".arw" || first["enabled"] != true || first["label"] != "Sony RAW" {
		t.Errorf("first row = %v", first)
	}
}

func TestSetImportTypes(t *testing.T) {
	t.Cleanup(func() { config.SetImportExtensions(nil) })
	fc := &fakeConfig{}
	srv := startServer(t, &fakeIngest{}, &fakeOutgest{}, &fakeStatus{}, fc)

	m := dialJSON(t, srv.SocketPath, `{"command":"set_import_types","types":["ARW","mp4","insv"]}`)
	if m["ok"] != true {
		t.Fatalf("set = %v", m)
	}
	rows := m["file_types"].([]any)
	enabled := map[string]bool{}
	for _, r := range rows {
		row := r.(map[string]any)
		if row["enabled"] == true {
			enabled[row["ext"].(string)] = true
		}
	}
	if len(enabled) != 3 || !enabled[".arw"] || !enabled[".mp4"] || !enabled[".insv"] {
		t.Errorf("enabled = %v", enabled)
	}

	for _, bad := range []string{`[]`, `[".xmp"]`, `["../x"]`} {
		m := dialJSON(t, srv.SocketPath, `{"command":"set_import_types","types":`+bad+`}`)
		if m["error"] != "invalid_file_type" {
			t.Errorf("types %s → %v", bad, m)
		}
	}
	if !config.IsImportable(".insv") {
		t.Error("rejected request changed the import set")
	}

	// null / missing types resets to defaults.
	m = dialJSON(t, srv.SocketPath, `{"command":"set_import_types","types":null}`)
	if m["ok"] != true || !config.IsImportable(".jpg") || config.IsImportable(".insv") {
		t.Errorf("reset = %v", m)
	}
}

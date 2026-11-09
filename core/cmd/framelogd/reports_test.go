package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
)

func TestReportProvider(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "c.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := db.InitDB(conn); err != nil {
		t.Fatal(err)
	}
	logger, err := logging.New(filepath.Join(dir, "l.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	backupDir := t.TempDir()
	p := &ingest.Pipeline{DB: conn, Logger: logger}
	p.SetBackupPath(backupDir)
	tr := &activity.Tracker{}
	rp := &reportProvider{tracker: tr, ingestPipeline: p, dbConn: conn}

	// Fresh install, empty library: nothing pending, no history.
	sum, err := rp.Backup()
	if err != nil || sum.Pending || sum.LastOutcome != "" {
		t.Fatalf("fresh Backup() = %+v, %v", sum, err)
	}
	if c, err := rp.LastCard(); err != nil || c != nil {
		t.Fatalf("fresh LastCard() = %v, %v", c, err)
	}

	// A photo with no backup yet → pending.
	db.InsertPhoto(conn, db.Photo{Hash: "h", ImportedPath: "/x"})
	if sum, _ := rp.Backup(); !sum.Pending {
		t.Error("photo never backed up should be pending")
	}

	db.InsertBackupRun(conn, db.BackupRun{FinishedAt: "2026-11-01T00:00:00Z",
		Destination: backupDir, Outcome: db.BackupOK, FilesCopied: 5})
	db.InsertBackupRun(conn, db.BackupRun{FinishedAt: "2026-11-02T00:00:00Z",
		Destination: backupDir, Outcome: db.BackupFailed, FilesFailed: 1, Error: "Backup drive is full"})
	sum, err = rp.Backup()
	if err != nil {
		t.Fatal(err)
	}
	want := struct {
		pending                         bool
		outcome, errText, last, success string
	}{true, "failed", "Backup drive is full", "2026-11-02T00:00:00Z", "2026-11-01T00:00:00Z"}
	if sum.Pending != want.pending || sum.LastOutcome != want.outcome || sum.LastError != want.errText ||
		sum.LastRunAt != want.last || sum.LastSuccessAt != want.success || sum.FilesFailed != 1 {
		t.Errorf("Backup() = %+v", sum)
	}

	db.InsertBackupRun(conn, db.BackupRun{FinishedAt: "2026-11-03T00:00:00Z",
		Destination: backupDir, Outcome: db.BackupOK})
	if sum, _ := rp.Backup(); sum.Pending {
		t.Error("successful latest run should clear pending")
	}
	p.SetBackupPath("")
	if sum, _ := rp.Backup(); sum.Pending {
		t.Error("backup disabled should never be pending")
	}

	tr.Start(activity.PhaseCopying, "Untitled", 3)
	if a := rp.Activity(); a.Phase != activity.PhaseCopying || a.Volume != "Untitled" {
		t.Errorf("Activity() = %+v", a)
	}
}

// Saving one setting must never reset another: before UpdateUserConfig,
// set_backup_path wrote a fresh UserConfig and would have dropped the
// import types.
func TestConfigSetter_SettingsDoNotClobberEachOther(t *testing.T) {
	orig := config.UserConfigPath
	config.UserConfigPath = filepath.Join(t.TempDir(), "framelog_config.json")
	t.Cleanup(func() {
		config.UserConfigPath = orig
		config.SetImportExtensions(nil)
	})

	dir := t.TempDir()
	conn, err := db.Open(filepath.Join(dir, "c.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cs := &configSetter{pipeline: &ingest.Pipeline{DB: conn}}

	if err := cs.SetImportTypes([]string{"ARW", "mp4"}); err != nil {
		t.Fatal(err)
	}
	if err := cs.SetBackupPath("/Volumes/Backup"); err != nil {
		t.Fatal(err)
	}
	got, err := config.ReadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupPath != "/Volumes/Backup" || len(got.ImportTypes) != 2 || got.ImportTypes[0] != ".arw" {
		t.Errorf("persisted = %+v", got)
	}
	if config.IsImportable(".jpg") || !config.IsImportable(".mp4") {
		t.Error("import set not applied")
	}

	// Invalid types are rejected before anything is persisted.
	if err := cs.SetImportTypes([]string{".xmp"}); !errors.Is(err, config.ErrInvalidFileType) {
		t.Errorf("err = %v", err)
	}
	if got, _ := config.ReadUserConfig(); len(got.ImportTypes) != 2 {
		t.Errorf("rejected change was persisted: %+v", got)
	}

	// nil resets to defaults and is stored as "never chosen".
	if err := cs.SetImportTypes(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.ReadUserConfig(); got.ImportTypes != nil || got.BackupPath != "/Volumes/Backup" {
		t.Errorf("after reset = %+v", got)
	}
	if !config.IsImportable(".jpg") {
		t.Error("defaults not restored")
	}
}

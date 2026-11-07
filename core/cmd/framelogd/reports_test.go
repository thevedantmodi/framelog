package main

import (
	"path/filepath"
	"testing"

	"github.com/thevedantmodi/framelog/core/activity"
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
		pending bool
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

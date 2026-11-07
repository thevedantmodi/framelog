package sdcard

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
	"github.com/thevedantmodi/framelog/core/ingest"
)

// importingRunner is a stand-in for ingest.Pipeline that does just enough
// for verification to have something real to check: every supported file in
// inbox/ is hashed, copied into originals/ (and the backup, when set), given
// a catalog row, and removed from inbox/.
type importingRunner struct {
	t          *testing.T
	inbox      string
	originals  string
	backupRoot string // "" = don't back up
	conn       *sql.DB
	skip       map[string]bool // base names to leave un-imported
	busyFor    int             // RunIngest returns AlreadyRunning this many times first

	mu    sync.Mutex
	calls int
}

func (r *importingRunner) Paused() bool { return false }

func (r *importingRunner) RunIngest() (ingest.Counts, error) {
	r.mu.Lock()
	r.calls++
	if r.busyFor > 0 {
		r.busyFor--
		r.mu.Unlock()
		return ingest.Counts{}, ingest.ErrIngestAlreadyRunning
	}
	r.mu.Unlock()

	var c ingest.Counts
	err := filepath.WalkDir(r.inbox, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !config.SupportedExtensions[strings.ToLower(filepath.Ext(p))] || r.skip[d.Name()] {
			return nil
		}
		h, err := hasher.HashFile(p)
		if err != nil {
			return err
		}
		body, _ := os.ReadFile(p)
		lib := filepath.Join(r.originals, "2026", h[:8]+strings.ToLower(filepath.Ext(p)))
		os.MkdirAll(filepath.Dir(lib), 0o755)
		os.WriteFile(lib, body, 0o644)
		if r.backupRoot != "" {
			bp := filepath.Join(backup.DestDir(r.backupRoot), "2026", filepath.Base(lib))
			os.MkdirAll(filepath.Dir(bp), 0o755)
			os.WriteFile(bp, body, 0o644)
		}
		if err := db.InsertPhoto(r.conn, db.Photo{Hash: h, ImportedPath: lib,
			ImportTimestamp: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			return err
		}
		c.Imported++
		return os.Remove(p)
	})
	return c, err
}

type verifyEnv struct {
	w       *Watcher
	runner  *importingRunner
	volumes string
	card    string
	backup  string
	reports []db.CardReport
}

func newVerifyEnv(t *testing.T, withBackup bool) *verifyEnv {
	t.Helper()
	root := t.TempDir()
	env := &verifyEnv{
		volumes: filepath.Join(root, "Volumes"),
		backup:  filepath.Join(root, "Volumes-backup", "Backup"),
	}
	env.card = filepath.Join(env.volumes, "Untitled")
	inbox := filepath.Join(root, "inbox")
	originals := filepath.Join(root, "originals")
	for _, d := range []string{env.volumes, inbox, originals, env.backup} {
		os.MkdirAll(d, 0o755)
	}
	conn, err := db.Open(filepath.Join(root, "catalog.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	db.InitDB(conn)

	binDir := t.TempDir()
	diskutil := writeFakeBin(t, binDir, "diskutil", `echo "   Removable Media:           Removable"
echo "   Volume UUID:               0E1D2C3B-AAAA-BBBB-CCCC-111122223333"`)

	backupPath := ""
	if withBackup {
		backupPath = env.backup
	}
	env.runner = &importingRunner{t: t, inbox: inbox, originals: originals,
		backupRoot: backupPath, conn: conn, skip: map[string]bool{}}
	env.w = &Watcher{
		DiskutilPath:        diskutil,
		RclonePath:          writeFakeRclone(t, binDir),
		VolumesRoot:         env.volumes,
		InboxPath:           inbox,
		Runner:              env.runner,
		Logger:              openTestLogger(t),
		DB:                  conn,
		OriginalsPath:       originals,
		ReportsDir:          filepath.Join(root, "reports"),
		BackupPath:          func() string { return backupPath },
		Activity:            &activity.Tracker{},
		IngestRetryInterval: time.Millisecond,
	}
	env.w.OnReport = func(r db.CardReport) { env.reports = append(env.reports, r) }
	return env
}

func (e *verifyEnv) put(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(e.card, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *verifyEnv) sonyCard(t *testing.T) {
	e.put(t, "DCIM/100MSDCF/DSC00001.ARW", "arw-1")
	e.put(t, "DCIM/100MSDCF/DSC00001.JPG", "jpg-1")
	e.put(t, "DCIM/100MSDCF/DSC00002.HIF", "hif-2")
	e.put(t, "PRIVATE/M4ROOT/CLIP/C0001.MP4", "mp4-1")
	e.put(t, "PRIVATE/M4ROOT/CLIP/C0001M01.XML", "<xml/>")
}

func TestProcessCard_HealthyCardReportsSafe(t *testing.T) {
	env := newVerifyEnv(t, true)
	env.sonyCard(t)

	env.w.processCard("Untitled", env.card)

	if len(env.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(env.reports))
	}
	r := env.reports[0]
	if !r.SafeToFormat {
		t.Fatalf("not safe: %+v", r)
	}
	if r.FilesTotal != 4 || r.FilesVerified != 4 || r.FilesNew != 4 {
		t.Errorf("total=%d verified=%d new=%d", r.FilesTotal, r.FilesVerified, r.FilesNew)
	}
	if r.VolumeUUID != "0E1D2C3B-AAAA-BBBB-CCCC-111122223333" {
		t.Errorf("VolumeUUID = %q", r.VolumeUUID)
	}
	if r.BackupState != "verified" {
		t.Errorf("BackupState = %q", r.BackupState)
	}
	if _, err := os.Stat(r.ReportPath); err != nil {
		t.Errorf("receipt not written: %v", err)
	}
	if ph := env.w.Activity.Snapshot().Phase; ph != activity.PhaseIdle {
		t.Errorf("activity after run = %q, want idle", ph)
	}
}

func TestProcessCard_FailedImportReportsDoNotFormat(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)
	env.runner.skip["DSC00002.HIF"] = true

	env.w.processCard("Untitled", env.card)

	r := env.reports[0]
	if r.SafeToFormat {
		t.Fatal("SafeToFormat with an un-imported file")
	}
	if r.FilesVerified != 3 || r.FilesProblem != 1 {
		t.Errorf("verified=%d problem=%d", r.FilesVerified, r.FilesProblem)
	}
}

func TestProcessCard_WaitsForRunningIngest(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)
	env.runner.busyFor = 3

	env.w.processCard("Untitled", env.card)

	if env.runner.calls != 4 {
		t.Errorf("RunIngest calls = %d, want 4 (3 busy + 1 real)", env.runner.calls)
	}
	if !env.reports[0].SafeToFormat {
		t.Errorf("card should be safe once the delayed ingest ran: %+v", env.reports[0])
	}
}

func TestProcessCard_GivesUpAfterRetryTimeout(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)
	env.runner.busyFor = 1 << 30
	env.w.IngestRetryTimeout = 20 * time.Millisecond

	env.w.processCard("Untitled", env.card)

	if env.reports[0].SafeToFormat {
		t.Error("nothing was imported, yet card reported safe")
	}
}

func TestVerifyMounted(t *testing.T) {
	env := newVerifyEnv(t, true)

	if _, err := env.w.VerifyMounted(); !errors.Is(err, ErrNoCard) {
		t.Fatalf("no card: err = %v", err)
	}

	env.sonyCard(t)
	env.w.processCard("Untitled", env.card)

	// Lose the backup copies (e.g. a different drive is plugged in now) —
	// a re-verify must notice without re-copying anything.
	os.RemoveAll(backup.DestDir(env.backup))
	sum, err := env.w.VerifyMounted()
	if err != nil {
		t.Fatal(err)
	}
	if sum.SafeToFormat || sum.BackupState != "incomplete" {
		t.Errorf("re-verify = safe %v backup %q", sum.SafeToFormat, sum.BackupState)
	}
	// Re-verify is not an import session: nothing is "new".
	if sum.FilesNew != 0 || sum.FilesExisting != 4 {
		t.Errorf("new=%d existing=%d", sum.FilesNew, sum.FilesExisting)
	}
	if env.runner.calls != 1 {
		t.Errorf("VerifyMounted must not ingest; RunIngest calls = %d", env.runner.calls)
	}
}

func TestVerifyMounted_BusyAndDisabled(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)

	env.w.cardMu.Lock()
	if _, err := env.w.VerifyMounted(); !errors.Is(err, ErrCardBusy) {
		t.Errorf("busy: err = %v", err)
	}
	env.w.cardMu.Unlock()

	env.w.DB = nil
	if _, err := env.w.VerifyMounted(); !errors.Is(err, ErrVerifyUnavailable) {
		t.Errorf("disabled: err = %v", err)
	}
}

func TestVolumeUUID(t *testing.T) {
	bin := t.TempDir()
	withUUID := writeFakeBin(t, bin, "du1", `echo "   Volume Name:   Untitled"
echo "   Volume UUID:   ABC-123"`)
	if got := VolumeUUID(withUUID, "/Volumes/x"); got != "ABC-123" {
		t.Errorf("VolumeUUID = %q", got)
	}
	none := writeFakeBin(t, bin, "du2", `echo "Removable Media: Removable"`)
	if got := VolumeUUID(none, "/Volumes/x"); got != "" {
		t.Errorf("VolumeUUID without line = %q", got)
	}
	failing := writeFakeBin(t, bin, "du3", "exit 1")
	if got := VolumeUUID(failing, "/Volumes/x"); got != "" {
		t.Errorf("VolumeUUID on failure = %q", got)
	}
}

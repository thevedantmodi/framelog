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
	"github.com/thevedantmodi/framelog/core/cardverify"
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
	t           *testing.T
	inbox       string
	originals   string
	backupRoot  string // "" = don't back up
	conn        *sql.DB
	skip        map[string]bool // base names to leave un-imported
	busyFor     int             // RunIngest returns AlreadyRunning this many times first
	noQuickHash bool            // simulate rows imported before quick fingerprints

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
		if !config.IsImportable(filepath.Ext(p)) || r.skip[d.Name()] {
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
		qh, _ := hasher.QuickHash(lib)
		if r.noQuickHash {
			qh = ""
		}
		if err := db.InsertPhoto(r.conn, db.Photo{Hash: h, ImportedPath: lib, QuickHash: qh,
			ImportTimestamp: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			return err
		}
		c.Imported++
		return os.Remove(p)
	})
	return c, err
}

// writeListingRclone is a fake rclone that honours --files-from-raw (copy
// exactly the listed paths) and otherwise copies the whole source tree, like
// the plain fake. Every copied path is appended to logPath so tests can
// assert what was actually read off the card.
func writeListingRclone(t *testing.T, dir, logPath string) string {
	t.Helper()
	return writeFakeBin(t, dir, "rclone", `
src="$2"; dst="$3"; list=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--files-from-raw" ]; then list="$a"; fi
  prev="$a"
done
mkdir -p "$dst"
copy_one() {
  rel="$1"; target="$dst/$rel"
  [ -e "$target" ] && return
  mkdir -p "$(dirname "$target")"
  cp "$src/$rel" "$target"
  echo "$rel" >> `+logPath+`
  printf '{"level":"info","msg":"Copied (new)","object":"%s"}\n' "$rel" >&2
}
if [ -n "$list" ]; then
  while IFS= read -r rel; do [ -n "$rel" ] && copy_one "$rel"; done < "$list"
else
  find "$src" -type f | while IFS= read -r f; do copy_one "${f#$src/}"; done
fi`)
}

type verifyEnv struct {
	w        *Watcher
	runner   *importingRunner
	volumes  string
	card     string
	backup   string
	reports  []db.CardReport
	previews []*cardverify.Preview
	copyLog  string
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
		RclonePath:          writeListingRclone(t, binDir, filepath.Join(root, "copied.log")),
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
	env.w.OnPreview = func(p *cardverify.Preview) { env.previews = append(env.previews, p) }
	env.copyLog = filepath.Join(root, "copied.log")
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

// copied returns the card paths the fake rclone copied since the last call.
func (e *verifyEnv) copied(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(e.copyLog)
	os.Remove(e.copyLog)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestProcessCard_PreviewThenCopiesOnlyNewFiles(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)

	// First insert: everything is new.
	env.w.processCard("Untitled", env.card)
	if len(env.previews) != 1 || len(env.previews[0].New) != 4 || env.previews[0].InLibrary != 0 {
		t.Fatalf("first preview = %+v", env.previews)
	}
	got := env.copied(t)
	if len(got) != 5 { // 4 media + the clip's XML sidecar
		t.Fatalf("first insert copied %v, want 4 media + sidecar", got)
	}
	if !env.reports[0].SafeToFormat {
		t.Fatalf("first insert not safe: %+v", env.reports[0])
	}
	if env.w.CurrentPreview() == nil {
		t.Error("preview should stay available while the card is mounted")
	}

	// Shoot two more frames and re-insert straight away: only those are
	// copied, and only those count as new even within the same second.
	env.put(t, "DCIM/100MSDCF/DSC00003.ARW", "arw-3")
	env.put(t, "PRIVATE/M4ROOT/CLIP/C0002.MP4", "mp4-2")
	env.put(t, "PRIVATE/M4ROOT/CLIP/C0002M01.XML", "<xml2/>")
	env.w.processCard("Untitled", env.card)

	p := env.previews[1]
	if len(p.New) != 2 || p.InLibrary != 4 {
		t.Fatalf("second preview: new=%d in=%d", len(p.New), p.InLibrary)
	}
	got = env.copied(t)
	want := map[string]bool{"100MSDCF/DSC00003.ARW": true, "C0002.MP4": true, "C0002M01.XML": true}
	if len(got) != len(want) {
		t.Fatalf("second insert copied %v, want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected copy %s", g)
		}
	}
	r := env.reports[1]
	if !r.SafeToFormat || r.FilesTotal != 6 || r.FilesNew != 2 || r.FilesExisting != 4 {
		t.Errorf("second report: safe=%v total=%d new=%d existing=%d",
			r.SafeToFormat, r.FilesTotal, r.FilesNew, r.FilesExisting)
	}
}

func TestProcessCard_NothingNewCopiesNothing(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)
	env.w.processCard("Untitled", env.card)
	env.copied(t)

	env.w.processCard("Untitled", env.card)
	if got := env.copied(t); len(got) != 0 {
		t.Errorf("re-inserting an imported card copied %v", got)
	}
	if !env.reports[1].SafeToFormat {
		t.Error("re-inserted card should still verify safe")
	}
}

// A quick fingerprint that matches a library row whose real content differs
// (the hint is wrong) must not cost a photo: verification notices the file
// isn't really imported, and the run copies it and checks again.
func TestProcessCard_WrongQuickMatchIsCaughtAndFixed(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.put(t, "DCIM/100MSDCF/DSC00001.ARW", "arw-1")
	qh, _ := hasher.QuickHash(filepath.Join(env.card, "DCIM/100MSDCF/DSC00001.ARW"))
	// A row with the same quick fingerprint but a different full hash.
	if err := db.InsertPhoto(env.runner.conn, db.Photo{Hash: "someotherphoto",
		ImportedPath: "/elsewhere/x.arw", QuickHash: qh}); err != nil {
		t.Fatal(err)
	}

	env.w.processCard("Untitled", env.card)

	if env.previews[0].InLibrary != 1 {
		t.Fatalf("setup: preview should have (wrongly) matched, got %+v", env.previews[0])
	}
	if got := env.copied(t); len(got) != 1 || got[0] != "100MSDCF/DSC00001.ARW" {
		t.Errorf("catch-up copy = %v", got)
	}
	if len(env.reports) != 1 {
		t.Fatalf("want exactly one saved report, got %d", len(env.reports))
	}
	if !env.reports[0].SafeToFormat {
		t.Errorf("after catch-up the card should verify safe: %+v", env.reports[0])
	}
}

func TestProcessCard_RowsWithoutQuickHashAreCopiedAgain(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.runner.noQuickHash = true
	env.sonyCard(t)
	env.w.processCard("Untitled", env.card)
	env.copied(t)

	// Pre-backfill rows aren't recognised: the preview calls them new and
	// they are copied again. Slower, still safe.
	env.w.processCard("Untitled", env.card)
	// The four media files are copied again (ingest removed them from
	// inbox/). The clip's XML is still in inbox/ — this fake importer never
	// consumes sidecars — so --ignore-existing skips it.
	if got := env.copied(t); len(got) != 4 {
		t.Errorf("copied %v, want the 4 media files again", got)
	}
	if !env.reports[1].SafeToFormat {
		t.Error("should still verify safe")
	}
}

func TestClearPreviewOnUnmount(t *testing.T) {
	env := newVerifyEnv(t, false)
	env.sonyCard(t)
	env.w.processCard("Untitled", env.card)
	if env.w.CurrentPreview() == nil {
		t.Fatal("no preview")
	}
	env.w.clearPreview("OtherCard")
	if env.w.CurrentPreview() == nil {
		t.Error("clearing another volume's preview removed this one")
	}
	env.w.clearPreview("Untitled")
	if env.w.CurrentPreview() != nil {
		t.Error("preview survived unmount")
	}
}

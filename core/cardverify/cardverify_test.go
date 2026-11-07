package cardverify

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
)

// fixture is a fake α7C II card plus a library and backup that hold copies
// of its files. Tests break one thing and assert the verdict.
type fixture struct {
	t         *testing.T
	card      string
	originals string
	backup    string
	conn      *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:         t,
		card:      filepath.Join(root, "Volumes", "Untitled"),
		originals: filepath.Join(root, "Photos", "originals"),
		backup:    filepath.Join(root, "Volumes", "Backup"),
	}
	for _, d := range []string{f.card, f.originals, f.backup} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Open(filepath.Join(root, "catalog.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.InitDB(conn); err != nil {
		t.Fatal(err)
	}
	f.conn = conn
	return f
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// onCard writes a card file only (not imported).
func (f *fixture) onCard(rel, content string) string {
	p := filepath.Join(f.card, rel)
	f.write(p, content)
	return p
}

// imported writes a card file, its library copy + catalog row, and its
// backup copy — the fully healthy state. Returns the library path.
func (f *fixture) imported(rel, content, libRel, importedAt string) string {
	f.t.Helper()
	cardPath := f.onCard(rel, content)
	h, err := hasher.HashFile(cardPath)
	if err != nil {
		f.t.Fatal(err)
	}
	lib := filepath.Join(f.originals, libRel)
	f.write(lib, content)
	f.write(filepath.Join(backup.DestDir(f.backup), libRel), content)
	model := "ILCE-7CM2"
	if err := db.InsertPhoto(f.conn, db.Photo{Hash: h, ImportedPath: lib,
		ImportTimestamp: importedAt, CaptureDate: "2026:11:06 18:00:00", CameraModel: &model}); err != nil {
		f.t.Fatal(err)
	}
	return lib
}

func (f *fixture) verify(backupPath string) *Report {
	f.t.Helper()
	rep, err := Verify(Options{
		VolumePath:    f.card,
		VolumeUUID:    "UUID-1",
		DB:            f.conn,
		OriginalsPath: f.originals,
		BackupPath:    backupPath,
		SessionStart:  time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		f.t.Fatalf("Verify: %v", err)
	}
	return rep
}

func (f *fixture) healthyCard() {
	f.imported("DCIM/100MSDCF/DSC00001.ARW", "raw-1", "2026/11/06/a_11111111.arw", "2026-11-07T10:00:05Z")
	f.imported("DCIM/100MSDCF/DSC00001.JPG", "jpg-1", "2026/11/06/a_22222222.jpg", "2026-11-07T10:00:06Z")
	f.imported("DCIM/100MSDCF/DSC00002.HIF", "hif-2", "2026/11/06/b_33333333.hif", "2026-11-01T09:00:00Z")
	f.imported("PRIVATE/M4ROOT/CLIP/C0001.MP4", "clip-1", "2026/11/06/c_44444444.mp4", "2026-11-07T10:00:07Z")
	f.onCard("PRIVATE/M4ROOT/CLIP/C0001M01.XML", "<xml/>")
}

func stateOf(t *testing.T, r *Report, rel string) string {
	t.Helper()
	for _, f := range r.Files {
		if f.RelPath == filepath.FromSlash(rel) {
			return f.State
		}
	}
	t.Fatalf("%s not in report", rel)
	return ""
}

func TestVerify_HealthyCardWithBackupIsSafe(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	// Mac-created noise must be ignored, not flagged as unhandled.
	f.onCard("DCIM/100MSDCF/._DSC00001.ARW", "appledouble")
	f.onCard(".Trashes/501/x", "trash")
	f.onCard("PRIVATE/M4ROOT/THMBNL/C0001T01.JPG", "thumb") // outside media dirs

	r := f.verify(f.backup)
	if !r.SafeToFormat {
		t.Fatalf("SafeToFormat = false; problems: %v", r.Problems)
	}
	if r.BackupState != BackupVerified {
		t.Errorf("BackupState = %q", r.BackupState)
	}
	if r.MediaCount() != 4 || r.Sidecars != 1 || len(r.Files) != 4 {
		t.Errorf("media=%d sidecars=%d files=%d", r.MediaCount(), r.Sidecars, len(r.Files))
	}
	if len(r.Problems) != 0 {
		t.Errorf("problems on clean card: %v", r.Problems)
	}

	sum := r.Summary()
	if sum.FilesVerified != 4 || sum.FilesProblem != 0 {
		t.Errorf("summary verified=%d problem=%d", sum.FilesVerified, sum.FilesProblem)
	}
	if sum.FilesNew != 3 || sum.FilesExisting != 1 {
		t.Errorf("new=%d existing=%d, want 3/1", sum.FilesNew, sum.FilesExisting)
	}
	if sum.FileTypes != "ARW 1 · HIF 1 · JPG 1 · MP4 1" {
		t.Errorf("FileTypes = %q", sum.FileTypes)
	}
	if sum.CameraModels != "ILCE-7CM2" {
		t.Errorf("CameraModels = %q", sum.CameraModels)
	}
}

func TestVerify_NoBackupConfiguredIsSafeWithNote(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	r := f.verify("")
	if !r.SafeToFormat || r.BackupState != BackupNotConfigured {
		t.Fatalf("safe=%v backup=%q problems=%v", r.SafeToFormat, r.BackupState, r.Problems)
	}
	if len(r.Notes) == 0 || !strings.Contains(r.Notes[0], "only copy") {
		t.Errorf("Notes = %v, want single-copy warning", r.Notes)
	}
}

func TestVerify_FileNotImportedBlocks(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	f.onCard("DCIM/100MSDCF/DSC00003.ARW", "never-imported")

	r := f.verify(f.backup)
	if r.SafeToFormat {
		t.Fatal("SafeToFormat = true with an un-imported file")
	}
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00003.ARW"); s != StateNotInLibrary {
		t.Errorf("state = %q", s)
	}
	if !strings.Contains(strings.Join(r.Problems, "|"), "1 file is not in the library") {
		t.Errorf("problems = %v", r.Problems)
	}
}

func TestVerify_CorruptLibraryCopyBlocks(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	lib := filepath.Join(f.originals, "2026/11/06/a_11111111.arw")
	f.write(lib, "raw-X") // same size, different bytes: only a hash catches this

	r := f.verify(f.backup)
	if r.SafeToFormat {
		t.Fatal("SafeToFormat = true with a corrupt library copy")
	}
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00001.ARW"); s != StateLibraryMismatch {
		t.Errorf("state = %q", s)
	}
}

func TestVerify_TruncatedAndMissingLibraryCopies(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	f.write(filepath.Join(f.originals, "2026/11/06/a_22222222.jpg"), "jp") // truncated
	os.Remove(filepath.Join(f.originals, "2026/11/06/b_33333333.hif"))

	r := f.verify(f.backup)
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00001.JPG"); s != StateLibraryMismatch {
		t.Errorf("truncated state = %q", s)
	}
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00002.HIF"); s != StateLibraryMissing {
		t.Errorf("missing state = %q", s)
	}
}

func TestVerify_BackupMissingAndCorrupt(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	dest := backup.DestDir(f.backup)
	os.Remove(filepath.Join(dest, "2026/11/06/a_11111111.arw"))
	f.write(filepath.Join(dest, "2026/11/06/c_44444444.mp4"), "clip-X")

	r := f.verify(f.backup)
	if r.SafeToFormat || r.BackupState != BackupIncomplete {
		t.Fatalf("safe=%v backup=%q", r.SafeToFormat, r.BackupState)
	}
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00001.ARW"); s != StateBackupMissing {
		t.Errorf("state = %q", s)
	}
	if s := stateOf(t, r, "PRIVATE/M4ROOT/CLIP/C0001.MP4"); s != StateBackupMismatch {
		t.Errorf("state = %q", s)
	}
}

func TestVerify_BackupDriveUnpluggedBlocks(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	r := f.verify(filepath.Join(t.TempDir(), "NotMounted"))
	if r.SafeToFormat || r.BackupState != BackupDriveMissing {
		t.Fatalf("safe=%v backup=%q", r.SafeToFormat, r.BackupState)
	}
	// Library checks still ran.
	if n := r.StateCounts()[StateVerified]; n != 4 {
		t.Errorf("verified = %d, want 4 (library-verified)", n)
	}
	if !strings.Contains(strings.Join(r.Problems, "|"), "Backup drive is not connected") {
		t.Errorf("problems = %v", r.Problems)
	}
}

func TestVerify_UnhandledFileTypeBlocks(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	f.onCard("DCIM/100MSDCF/DSC00009.XYZ", "mystery")

	r := f.verify(f.backup)
	if r.SafeToFormat {
		t.Fatal("SafeToFormat = true with an unhandled file on the card")
	}
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00009.XYZ"); s != StateUnhandled {
		t.Errorf("state = %q", s)
	}
	if !strings.Contains(strings.Join(r.Problems, "|"), "DSC00009.XYZ") {
		t.Errorf("problems should name an example: %v", r.Problems)
	}
}

func TestVerify_EmptyCardIsSafe(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Join(f.card, "DCIM"), 0o755)
	r := f.verify(f.backup)
	if !r.SafeToFormat {
		t.Fatalf("empty card not safe: %v", r.Problems)
	}
}

func TestVerify_ExtraProblemsAreReportedButDoNotDecide(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	rep, err := Verify(Options{VolumePath: f.card, DB: f.conn, OriginalsPath: f.originals,
		ExtraProblems: []string{"DCIM copy: rclone exited 1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.SafeToFormat {
		t.Error("an rclone error that left every file verified should not block")
	}
	if len(rep.Problems) != 1 || rep.Problems[0] != "DCIM copy: rclone exited 1" {
		t.Errorf("Problems = %v", rep.Problems)
	}
}

func TestVerify_ProgressReportsEveryMediaFile(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	f.onCard("DCIM/100MSDCF/DSC00003.ARW", "never-imported")
	var calls int
	var lastTotal int
	_, err := Verify(Options{VolumePath: f.card, DB: f.conn, OriginalsPath: f.originals,
		Progress: func(done, total int, file string) {
			calls++
			lastTotal = total
		}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 5 || lastTotal != 5 {
		t.Errorf("progress calls=%d total=%d, want 5/5", calls, lastTotal)
	}
}

func TestSave_WritesReceiptAndRow(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	f.onCard("DCIM/100MSDCF/DSC00003.ARW", "never-imported")
	r := f.verify(f.backup)

	reports := filepath.Join(t.TempDir(), "reports")
	sum, err := Save(f.conn, r, reports)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ID == 0 || sum.ReportPath == "" {
		t.Fatalf("sum = %+v", sum)
	}
	body, err := os.ReadFile(sum.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"DO NOT FORMAT", "Untitled", "not_in_library", "DSC00003.ARW",
		"ILCE-7CM2", "Backup:", "4/5 verified"} {
		if !strings.Contains(text, want) {
			t.Errorf("receipt missing %q:\n%s", want, text)
		}
	}
	// Problem rows are listed before verified ones.
	if strings.Index(text, "not_in_library") > strings.Index(text, "verified  ") {
		t.Errorf("problem rows should come first:\n%s", text)
	}

	last, err := db.LastCardReport(f.conn)
	if err != nil || last == nil || last.ID != sum.ID || last.SafeToFormat {
		t.Fatalf("LastCardReport = %+v, %v", last, err)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 45_300_000: "45.3 MB", 64_000_000_000: "64.0 GB"}
	for n, want := range cases {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestReceiptName(t *testing.T) {
	r := &Report{VolumeName: "NO NAME/x", StartedAt: time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)}
	if got := ReceiptName(r); got != "20261107_100000_NO_NAME_x.txt" {
		t.Errorf("ReceiptName = %q", got)
	}
}

func TestWriteReceipt_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	r := &Report{VolumeName: "Untitled", StartedAt: time.Now(), FinishedAt: time.Now()}
	a, err := WriteReceipt(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := WriteReceipt(r, dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasSuffix(b, "-2.txt") {
		t.Errorf("second receipt %q should not reuse %q", b, a)
	}
}

func TestVerify_NoSessionMeansNothingNew(t *testing.T) {
	f := newFixture(t)
	f.healthyCard()
	rep, err := Verify(Options{VolumePath: f.card, DB: f.conn, OriginalsPath: f.originals})
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Summary(); s.FilesNew != 0 || s.FilesExisting != 4 {
		t.Errorf("new=%d existing=%d, want 0/4", s.FilesNew, s.FilesExisting)
	}
}

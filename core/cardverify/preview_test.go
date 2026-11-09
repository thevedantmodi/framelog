package cardverify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
)

func TestPreviewCard(t *testing.T) {
	f := newFixture(t)
	f.healthyCard() // 4 media files, imported via InsertPhoto (no quick hash yet)
	f.onCard("DCIM/100MSDCF/DSC00003.ARW", "brand-new")
	f.onCard("DCIM/100MSDCF/DSC00009.XYZ", "mystery")

	// Before the backfill, nothing has a quick fingerprint: all 5 look new.
	p, err := PreviewCard(f.card, "", f.conn)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != 5 || len(p.New) != 5 || p.InLibrary != 0 {
		t.Fatalf("before backfill: files=%d new=%d in=%d", p.Files, len(p.New), p.InLibrary)
	}
	if p.Unhandled != 1 || p.Sidecars != 1 || p.VolumeName != "Untitled" {
		t.Errorf("unhandled=%d sidecars=%d name=%q", p.Unhandled, p.Sidecars, p.VolumeName)
	}

	n, err := BackfillQuickHashes(f.conn, nil)
	if err != nil || n != 4 {
		t.Fatalf("backfill = %d, %v", n, err)
	}
	// Idempotent: nothing left to fill.
	if n, _ := BackfillQuickHashes(f.conn, nil); n != 0 {
		t.Errorf("second backfill processed %d rows", n)
	}

	p, err = PreviewCard(f.card, "", f.conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.New) != 1 || p.InLibrary != 4 {
		t.Fatalf("after backfill: new=%d in=%d", len(p.New), p.InLibrary)
	}
	if p.New[0].RelPath != filepath.FromSlash("DCIM/100MSDCF/DSC00003.ARW") {
		t.Errorf("new file = %s", p.New[0].RelPath)
	}
	if p.NewTypes() != "ARW 1" {
		t.Errorf("NewTypes = %q", p.NewTypes())
	}
	if got := p.Summary(); got == "" {
		t.Error("empty summary")
	}
}

func TestBackfill_UnreadableFileIsNotRetried(t *testing.T) {
	f := newFixture(t)
	if err := db.InsertPhoto(f.conn, db.Photo{Hash: "gone", ImportedPath: "/nonexistent/x.arw"}); err != nil {
		t.Fatal(err)
	}
	if n, err := BackfillQuickHashes(f.conn, nil); err != nil || n != 1 {
		t.Fatalf("backfill = %d, %v", n, err)
	}
	rows, _ := db.PhotosWithoutQuickHash(f.conn, 10)
	if len(rows) != 0 {
		t.Errorf("unreadable row would be retried: %+v", rows)
	}
}

func TestPreview_IgnoredTypes(t *testing.T) {
	t.Cleanup(func() { config.SetImportExtensions(nil) })
	if err := config.SetImportExtensions([]string{".arw", ".mp4"}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.onCard("DCIM/100MSDCF/DSC00001.ARW", "raw")
	f.onCard("DCIM/100MSDCF/DSC00001.JPG", "jpg")

	p, err := PreviewCard(f.card, "", f.conn)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != 1 || p.Ignored != 1 || p.Unhandled != 0 {
		t.Errorf("files=%d ignored=%d unhandled=%d", p.Files, p.Ignored, p.Unhandled)
	}

	// Verification: the switched-off JPG is listed, noted, and does not block.
	r := f.verify("")
	if s := stateOf(t, r, "DCIM/100MSDCF/DSC00001.JPG"); s != StateIgnored {
		t.Errorf("JPG state = %q", s)
	}
	if len(r.Notes) == 0 {
		t.Error("expected a note about the skipped JPG")
	}
	if sum := r.Summary(); sum.FilesIgnored != 1 || sum.FilesProblem != 1 {
		// The ARW is not imported in this fixture, so it is the one problem.
		t.Errorf("ignored=%d problem=%d", sum.FilesIgnored, sum.FilesProblem)
	}
}

func TestPreview_QuickHashFromIngestMatches(t *testing.T) {
	// The fingerprint stored at import (of the inbox copy) must equal the one
	// computed from the card, or the preview would never recognise anything.
	dir := t.TempDir()
	a := filepath.Join(dir, "card.arw")
	b := filepath.Join(dir, "inbox.arw")
	os.WriteFile(a, []byte("same bytes"), 0o644)
	os.WriteFile(b, []byte("same bytes"), 0o644)
	ha, _ := hasher.QuickHash(a)
	hb, _ := hasher.QuickHash(b)
	if ha != hb {
		t.Error("quick fingerprint depends on more than content")
	}
}

package sim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thevedantmodi/framelog/core/cardverify"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
)

// scenario is one story from a shoot. Scenarios run in order and build on
// each other: the card and library carry over from one to the next.
type scenario struct {
	id, title, story string
	// needsFakeRclone marks scenarios that make rclone fail on purpose.
	needsFakeRclone bool
	run             func(s *sim, c *check) error
}

// check collects failed expectations and narration for one scenario.
type check struct {
	failures, notes []string
}

func (c *check) expect(ok bool, format string, args ...any) {
	if !ok {
		c.failures = append(c.failures, fmt.Sprintf(format, args...))
	}
}

func (c *check) note(format string, args ...any) {
	c.notes = append(c.notes, fmt.Sprintf(format, args...))
}

func (s *sim) runScenario(n int, sc scenario) Result {
	r := Result{ID: sc.id, Title: sc.title}
	s.say("▶ %d. %s  [%s]\n   %s\n", n, sc.title, sc.id, sc.story)
	if sc.needsFakeRclone && s.tools.realRclone {
		r.Skipped = true
		s.say("   – skipped: needs the fake rclone to simulate a full drive\n\n")
		return r
	}
	start := time.Now()
	c := &check{}
	if err := sc.run(s, c); err != nil {
		c.failures = append(c.failures, err.Error())
	}
	r.Duration = time.Since(start)
	r.Notes, r.Failures = c.notes, c.failures
	r.Passed = len(c.failures) == 0
	for _, note := range c.notes {
		s.say("   · %s\n", note)
	}
	for _, f := range c.failures {
		s.say("   ✗ %s\n", f)
	}
	if r.Passed {
		s.say("   ✓ passed (%s)\n\n", r.Duration.Round(10*time.Millisecond))
	} else {
		s.say("   FAILED\n\n")
	}
	return r
}

// ---- narration helpers --------------------------------------------------------

func describePreview(c *check, p *cardverify.Preview) {
	if p == nil {
		c.note("preview: none")
		return
	}
	line := fmt.Sprintf("preview: %d new, %d already imported", len(p.New), p.InLibrary)
	if p.Ignored > 0 {
		line += fmt.Sprintf(", %d skipped by File Types", p.Ignored)
	}
	c.note("%s", line)
}

func describeReport(c *check, r db.CardReport) {
	verdict := "DO NOT FORMAT"
	if r.SafeToFormat {
		verdict = "SAFE TO FORMAT"
	}
	c.note("verdict: %s — %d/%d verified, backup %s", verdict, r.FilesVerified, r.FilesTotal, r.BackupState)
	for _, p := range r.Problems {
		c.note("  problem: %s", p)
	}
}

// verifyViaSocket runs the menu bar's "Verify Card Now" and returns the card.
func (s *sim) verifyViaSocket(c *check) (map[string]any, error) {
	// The card run that just reported may still be releasing the card for
	// a moment; the menu shows "already being checked" in that case, and a
	// person would simply press again.
	var m map[string]any
	var err error
	for i := 0; i < 50; i++ {
		if m, err = s.command("verify_card"); err != nil {
			return nil, err
		}
		if m["error"] != "card_busy" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.drainReports() // Verify Card Now saves a report too; don't let insert() mistake it for the next run's
	card, ok := m["card"].(map[string]any)
	if m["ok"] != true || !ok {
		return nil, fmt.Errorf("verify_card failed: %v", m)
	}
	safe := card["safe_to_format"] == true
	c.note("Verify Card Now → safe to format: %v", safe)
	return card, nil
}

func problemsOf(card map[string]any) string {
	var out []string
	for _, p := range card["problems"].([]any) {
		out = append(out, fmt.Sprint(p))
	}
	return strings.Join(out, " | ")
}

// ---- the scenarios ------------------------------------------------------------

var scenarios = []scenario{
	{
		id:    "fresh-card",
		title: "First import of a fresh card",
		story: "A card with 2 RAW+JPEG pairs, 2 HEIF stills and 2 XAVC clips goes in.",
		run: func(s *sim, c *check) error {
			p, r, err := s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 8 && p.InLibrary == 0, "preview should find 8 new files")
			c.expect(r.SafeToFormat, "card should be safe to format")
			c.expect(r.FilesTotal == 8 && r.FilesVerified == 8, "want 8/8 verified, got %d/%d", r.FilesVerified, r.FilesTotal)
			c.expect(r.FilesNew == 8, "want 8 newly imported, got %d", r.FilesNew)
			c.expect(r.BackupState == cardverify.BackupVerified, "backup state = %q", r.BackupState)
			c.expect(r.CameraModels == "ILCE-7CM2", "camera = %q", r.CameraModels)
			if _, err := os.Stat(r.ReportPath); err != nil {
				c.expect(false, "receipt missing: %v", err)
			} else {
				c.note("receipt: %s", filepath.Base(r.ReportPath))
			}

			// What the menu bar would show right now.
			st, err := s.command("status")
			if err != nil {
				return err
			}
			c.expect(st["photo_count"] == float64(8), "status photo_count = %v", st["photo_count"])
			lc, _ := st["last_card"].(map[string]any)
			c.expect(lc != nil && lc["safe_to_format"] == true, "status last_card = %v", st["last_card"])
			cp, _ := st["card_preview"].(map[string]any)
			c.expect(cp != nil && cp["files_new"] == float64(8), "status card_preview = %v", st["card_preview"])
			c.note("menu bar status: %v photos, last card safe = %v", st["photo_count"], lc["safe_to_format"])
			return nil
		},
	},
	{
		id:    "same-card-again",
		title: "Re-inserting a card that's already imported",
		story: "You eject the card and put it back in. Nothing should be copied again.",
		run: func(s *sim, c *check) error {
			if err := s.eject(); err != nil {
				return err
			}
			p, r, err := s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 0 && p.InLibrary == 8, "preview should find nothing new")
			c.expect(s.duplicatesParked() == 0, "files were copied again (%d parked as duplicates)", s.duplicatesParked())
			c.expect(r.SafeToFormat, "card should still be safe to format")
			c.expect(r.FilesExisting == 8 && r.FilesNew == 0, "want 0 new / 8 existing, got %d / %d", r.FilesNew, r.FilesExisting)
			return nil
		},
	},
	{
		id:    "more-shots",
		title: "Shooting more on the same card",
		story: "You shoot one more RAW and one more clip, then insert the card again.",
		run: func(s *sim, c *check) error {
			if err := s.eject(); err != nil {
				return err
			}
			if err := s.shoot("ARW"); err != nil {
				return err
			}
			if err := s.film(); err != nil {
				return err
			}
			p, r, err := s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 2, "preview should find exactly the 2 new files")
			c.expect(r.SafeToFormat && r.FilesTotal == 10, "want safe with 10 files, got safe=%v total=%d", r.SafeToFormat, r.FilesTotal)
			c.expect(r.FilesNew == 2, "want 2 newly imported, got %d", r.FilesNew)
			return nil
		},
	},
	{
		id:    "backup-unplugged",
		title: "Backup drive not plugged in",
		story: "You insert the card with the backup drive unplugged, then plug it in and press Verify Card Now.",
		run: func(s *sim, c *check) error {
			if err := s.eject(); err != nil {
				return err
			}
			if err := os.Rename(s.backupDir, s.backupUnplugged); err != nil {
				return err
			}
			_, r, err := s.insert()
			if err != nil {
				return err
			}
			describeReport(c, r)
			c.expect(!r.SafeToFormat, "must NOT be safe to format with the backup drive missing")
			c.expect(r.BackupState == cardverify.BackupDriveMissing, "backup state = %q", r.BackupState)
			c.expect(strings.Contains(problemsText(r), "not connected"), "problem should say the drive isn't connected")

			if err := os.Rename(s.backupUnplugged, s.backupDir); err != nil {
				return err
			}
			c.note("backup drive plugged back in")
			card, err := s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == true, "should be safe once the drive is back: %s", problemsOf(card))
			return nil
		},
	},
	{
		id:              "backup-full",
		title:           "Backup drive is full",
		story:           "The backup drive fills up during a card import. Then you free space and press Retry Backup.",
		needsFakeRclone: true,
		run: func(s *sim, c *check) error {
			if err := s.eject(); err != nil {
				return err
			}
			if err := os.WriteFile(s.fullMarker, nil, 0o644); err != nil {
				return err
			}
			if err := s.shoot("ARW"); err != nil {
				return err
			}
			_, r, err := s.insert()
			if err != nil {
				return err
			}
			describeReport(c, r)
			c.expect(!r.SafeToFormat, "must NOT be safe to format when the backup failed")
			c.expect(r.BackupState == cardverify.BackupIncomplete, "backup state = %q", r.BackupState)

			st, err := s.command("status")
			if err != nil {
				return err
			}
			b, _ := st["backup"].(map[string]any)
			c.expect(b != nil && b["last_outcome"] == "failed", "status backup = %v", st["backup"])
			c.expect(b != nil && b["last_error"] == "Backup drive is full", "backup error = %v", b["last_error"])
			c.expect(b != nil && b["pending"] == true, "backup should be pending")
			if b != nil {
				c.note("menu bar shows: ⚠ Backup failed: %v", b["last_error"])
			}

			os.Remove(s.fullMarker)
			c.note("space freed")
			m, err := s.command("backup_now")
			if err != nil {
				return err
			}
			c.expect(m["ok"] == true && m["outcome"] == "ok", "Retry Backup = %v", m)
			c.note("Retry Backup → %v", m["outcome"])
			card, err := s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == true, "should be safe after the retry: %s", problemsOf(card))
			return nil
		},
	},
	{
		id:    "corrupt-library-copy",
		title: "A library copy gets corrupted",
		story: "One byte in the middle of an imported RAW changes on disk. Verify Card Now must notice.",
		run: func(s *sim, c *check) error {
			lib, err := s.libraryFile(".arw")
			if err != nil {
				return err
			}
			orig, err := os.ReadFile(lib)
			if err != nil {
				return err
			}
			bad := append([]byte(nil), orig...)
			bad[len(bad)/2] ^= 0xFF // same size, middle byte — a quick hash can't see this
			if err := os.WriteFile(lib, bad, 0o644); err != nil {
				return err
			}
			c.note("flipped one byte in %s", filepath.Base(lib))
			card, err := s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == false, "a corrupt library copy must block formatting")
			c.expect(strings.Contains(problemsOf(card), "byte-for-byte"), "problem should name the mismatch: %s", problemsOf(card))
			c.note("problem: %s", problemsOf(card))

			if err := os.WriteFile(lib, orig, 0o644); err != nil {
				return err
			}
			c.note("library file restored")
			card, err = s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == true, "should be safe once restored: %s", problemsOf(card))
			return nil
		},
	},
	{
		id:    "unknown-file-type",
		title: "A file type Framelog doesn't know",
		story: "Something Framelog can't import appears in DCIM/. Formatting would destroy it.",
		run: func(s *sim, c *check) error {
			odd := filepath.Join(s.cardDir(), "DCIM/100MSDCF/DSC09999.XYZ")
			if err := s.write(odd, 1_000); err != nil {
				return err
			}
			card, err := s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == false, "an unknown file must block formatting")
			c.expect(strings.Contains(problemsOf(card), "DSC09999.XYZ"), "problem should name the file: %s", problemsOf(card))
			c.note("problem: %s", problemsOf(card))
			if err := os.Remove(odd); err != nil {
				return err
			}
			card, err = s.verifyViaSocket(c)
			if err != nil {
				return err
			}
			c.expect(card["safe_to_format"] == true, "should be safe once removed: %s", problemsOf(card))
			return nil
		},
	},
	{
		id:    "jpeg-switched-off",
		title: "Switching JPEG off in File Types",
		story: "You only keep RAW, so you switch JPEG off and shoot another RAW+JPEG pair. Then you switch it back on.",
		run: func(s *sim, c *check) error {
			var types []string
			for _, ext := range []string{".arw", ".hif", ".cr3", ".cr2", ".nef", ".raf", ".orf", ".rw2",
				".dng", ".heic", ".mp4", ".mov", ".wav"} {
				types = append(types, fmt.Sprintf("%q", ext))
			}
			m, err := s.call(`{"command":"set_import_types","types":[` + strings.Join(types, ",") + `]}`)
			if err != nil {
				return err
			}
			c.expect(m["ok"] == true, "set_import_types = %v", m)
			c.note("File Types: JPEG switched off")

			if err := s.eject(); err != nil {
				return err
			}
			if err := s.shoot("ARW", "JPG"); err != nil {
				return err
			}
			p, r, err := s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 1 && p.Ignored == 3, "preview should find 1 new RAW and skip 3 JPEGs")
			c.expect(r.SafeToFormat, "skipped JPEGs must not block — you chose to drop them")
			c.expect(r.FilesIgnored == 3, "want 3 ignored, got %d", r.FilesIgnored)

			if _, err := s.call(`{"command":"set_import_types","types":null}`); err != nil {
				return err
			}
			c.note("File Types: reset to defaults, card re-inserted")
			if err := s.eject(); err != nil {
				return err
			}
			p, r, err = s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 1, "the never-imported JPEG should now be new")
			c.expect(r.SafeToFormat && r.FilesIgnored == 0, "want safe with nothing ignored")
			return nil
		},
	},
	{
		id:    "wrong-quick-match",
		title: "The quick check guesses wrong",
		story: "A new photo's quick fingerprint happens to match a different library photo. The full check must catch it and import it anyway.",
		run: func(s *sim, c *check) error {
			if err := s.eject(); err != nil {
				return err
			}
			if err := s.shoot("ARW"); err != nil {
				return err
			}
			newest := filepath.Join(s.cardDir(), fmt.Sprintf("DCIM/100MSDCF/DSC%05d.ARW", s.nextShot-1))
			qh, err := hasher.QuickHash(newest)
			if err != nil {
				return err
			}
			decoy := db.Photo{Hash: "decoy-not-this-photo", ImportedPath: "/nowhere/decoy.arw", QuickHash: qh}
			if err := db.InsertPhoto(s.conn, decoy); err != nil {
				return err
			}
			defer s.conn.Exec("DELETE FROM photos WHERE hash = ?", decoy.Hash) //nolint:errcheck

			p, r, err := s.insert()
			if err != nil {
				return err
			}
			describePreview(c, p)
			describeReport(c, r)
			c.expect(p != nil && len(p.New) == 0, "setup: the preview should have been fooled")
			c.expect(r.SafeToFormat, "the full check should catch the miss, import the photo, and end safe")
			var n int
			s.conn.QueryRow("SELECT COUNT(*) FROM photos WHERE original_filename = ?", filepath.Base(newest)).Scan(&n) //nolint:errcheck
			c.expect(n == 1, "the photo should be in the library (rows = %d)", n)
			return nil
		},
	},
}

// IDs lists every scenario ID in run order.
func IDs() []string {
	out := make([]string, len(scenarios))
	for i, sc := range scenarios {
		out[i] = sc.id
	}
	return out
}

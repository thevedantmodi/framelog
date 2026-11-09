package cardverify

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/hasher"
)

// Preview is the instant answer to "what on this card isn't in my library
// yet?", computed from quick fingerprints (hasher.QuickHash) in seconds,
// before anything is copied. It decides what gets copied; it never decides
// whether the card is safe to format — Verify does that with full hashes.
type Preview struct {
	VolumeName string
	VolumePath string
	CheckedAt  time.Time

	Files     int   // importable media files on the card
	Bytes     int64 // their total size
	New       []CardFile
	NewBytes  int64
	InLibrary int // media files whose quick fingerprint is already in the catalog
	Ignored   int // known types switched off in File Types
	Unhandled int // types Framelog doesn't recognise
	Sidecars  int // Sony clip .xml files (copied with their clips)
}

// NewTypes renders the new files' types, e.g. "ARW 156 · JPG 156".
func (p *Preview) NewTypes() string {
	types := map[string]int{}
	for _, f := range p.New {
		types[strings.ToUpper(strings.TrimPrefix(filepath.Ext(f.RelPath), "."))]++
	}
	return formatTypes(types)
}

// Summary is the one-line log form.
func (p *Preview) Summary() string {
	if p.Files == 0 {
		return "no photos or videos on the card"
	}
	if len(p.New) == 0 {
		return fmt.Sprintf("all %d files already in library", p.Files)
	}
	return fmt.Sprintf("%d new (%s, %s), %d already in library",
		len(p.New), HumanBytes(p.NewBytes), p.NewTypes(), p.InLibrary)
}

// PreviewCard inventories the card and checks each media file's quick
// fingerprint against the catalog. A file that can't be read counts as new:
// copying it is the safe choice, and verification will report the read
// error if it persists.
func PreviewCard(volPath, volumeName string, conn *sql.DB) (*Preview, error) {
	inv, err := Inventory(volPath)
	if err != nil {
		return nil, err
	}
	if volumeName == "" {
		volumeName = filepath.Base(volPath)
	}
	p := &Preview{VolumeName: volumeName, VolumePath: volPath}
	for _, f := range inv {
		switch f.Kind {
		case KindSidecar:
			p.Sidecars++
			continue
		case KindIgnored:
			p.Ignored++
			continue
		case KindUnhandled:
			p.Unhandled++
			continue
		}
		p.Files++
		p.Bytes += f.Size
		known := false
		if qh, err := hasher.QuickHash(f.AbsPath); err == nil {
			known, err = db.QuickHashExists(conn, qh)
			if err != nil {
				return nil, fmt.Errorf("cardverify: preview lookup: %w", err)
			}
		}
		if known {
			p.InLibrary++
		} else {
			p.New = append(p.New, f)
			p.NewBytes += f.Size
		}
	}
	p.CheckedAt = time.Now()
	return p, nil
}

// BackfillQuickHashes fills in quick fingerprints for photos imported before
// they existed, reading 128 KiB from each library file. Until a row is
// filled, the preview treats that photo as new and the card copy fetches it
// again (ingest then parks it as a duplicate) — slower, never unsafe. A file
// that can't be read gets "" so it is not retried forever. Returns how many
// rows were processed; stops early when stop is closed.
func BackfillQuickHashes(conn *sql.DB, stop <-chan struct{}) (int, error) {
	const batch = 500
	n := 0
	for {
		rows, err := db.PhotosWithoutQuickHash(conn, batch)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		for _, r := range rows {
			select {
			case <-stop:
				return n, nil
			default:
			}
			qh, err := hasher.QuickHash(r.ImportedPath)
			if err != nil {
				qh = ""
			}
			if err := db.SetQuickHash(conn, r.Hash, qh); err != nil {
				return n, err
			}
			n++
		}
	}
}

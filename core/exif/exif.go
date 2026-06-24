// Package exif extracts metadata from photo files via exiftool. The lookup and
// the read are intentionally two separate functions so tests can inject a fake
// binary path into ReadExif without any PATH manipulation or exec mocking —
// this fixes the exact bug class found in the Python predecessor's test_exif.py,
// which mocked subprocess output but never mocked binary resolution, silently
// requiring a real exiftool install on every test machine.
package exif

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Metadata holds the fields this pipeline reads from a photo's EXIF data.
// Types match db.Photo exactly — nullable fields are pointers so absent data
// is nil rather than a zero value. The Python predecessor read GPS coordinates
// and silently dropped them before they reached the database; pointer types
// here make "not present" unambiguous at the type level.
type Metadata struct {
	CaptureDate string   // always set; falls back to file mtime if no EXIF date
	CameraModel *string  // nil when exiftool returns no Model field
	GPSLat      *float64 // nil when GPS absent — not 0.0, which is a real coordinate
	GPSLon      *float64
}

// candidatePaths is the ordered list of known exiftool install locations on
// macOS, matching what the Python predecessor checked. Homebrew arm64 first,
// then Intel Homebrew, then system. Package-level var (not const) so the
// FindExiftool test can replace it to force the LookPath fallback.
var candidatePaths = []string{
	"/opt/homebrew/bin/exiftool",
	"/usr/local/bin/exiftool",
	"/usr/bin/exiftool",
}

// FindExiftool returns the absolute path to the exiftool binary. It checks the
// known Homebrew/system locations first, then falls back to exec.LookPath for
// any other directory on PATH. Returns an actionable error message if nothing
// is found.
func FindExiftool() (string, error) {
	for _, p := range candidatePaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("exiftool"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("exiftool not found; install it with: brew install exiftool")
}

// exiftoolJSON is the per-file JSON shape exiftool emits with the -json flag.
// Only the fields we use are named — unknown fields are silently ignored.
type exiftoolJSON struct {
	Model            string   `json:"Model"`
	DateTimeOriginal string   `json:"DateTimeOriginal"`
	GPSLatitude      *float64 `json:"GPSLatitude"`
	GPSLongitude     *float64 `json:"GPSLongitude"`
}

// ExifTimeFormat is exiftool's DateTimeOriginal layout. Used for both parsing
// exiftool output and for formatting the mtime fallback so callers always see
// the same layout regardless of whether real EXIF data was present.
const ExifTimeFormat = "2006:01:02 15:04:05"

// ReadExif runs exiftoolPath with the -json flag against path and returns the
// parsed Metadata. The binary path is an explicit parameter rather than being
// resolved internally — this is what makes the function testable without PATH
// manipulation: pass a fake shell script in a t.TempDir() directly.
//
// If DateTimeOriginal is missing or unparseable, CaptureDate falls back to the
// file's mtime formatted with ExifTimeFormat. If the subprocess exits non-zero,
// the error includes stderr so the cause is visible without re-running manually.
func ReadExif(path, exiftoolPath string) (Metadata, error) {
	var stderr bytes.Buffer
	// -n suppresses exiftool's print conversions so GPS fields arrive as decimal
	// float64 rather than DMS strings like "25 deg 12' 34.56\" N". Without -n,
	// cameras (notably DJI drones) that store GPS in the composite DMS tag return
	// a string exiftoolJSON cannot unmarshal into *float64.
	cmd := exec.Command(exiftoolPath, "-json", "-n", path)
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return Metadata{}, fmt.Errorf("exiftool: %w; stderr: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}

	var rows []exiftoolJSON
	if err := json.Unmarshal(out, &rows); err != nil {
		return Metadata{}, fmt.Errorf("exiftool: parse JSON: %w", err)
	}
	if len(rows) == 0 {
		return Metadata{}, fmt.Errorf("exiftool: empty output for %s", path)
	}

	row := rows[0]
	var meta Metadata

	// CaptureDate: parse exiftool's layout; fall back to mtime on missing/malformed.
	if row.DateTimeOriginal != "" {
		if t, err := time.ParseInLocation(ExifTimeFormat, row.DateTimeOriginal, time.Local); err == nil {
			meta.CaptureDate = t.Format(ExifTimeFormat)
		}
	}
	if meta.CaptureDate == "" {
		fi, err := os.Stat(path)
		if err != nil {
			return Metadata{}, fmt.Errorf("exif: stat for mtime fallback: %w", err)
		}
		meta.CaptureDate = fi.ModTime().Format(ExifTimeFormat)
	}

	// CameraModel: nil when the field is absent or empty.
	if row.Model != "" {
		s := row.Model
		meta.CameraModel = &s
	}

	// GPS: the exiftoolJSON struct already uses *float64 so absent fields arrive
	// as nil here — no extra check needed.
	meta.GPSLat = row.GPSLatitude
	meta.GPSLon = row.GPSLongitude

	return meta, nil
}

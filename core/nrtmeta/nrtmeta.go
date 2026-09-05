// Package nrtmeta parses the Sony NonRealTimeMeta XML sidecar that sits next
// to every clip in PRIVATE/M4ROOT/CLIP on an XAVC card (C0001.MP4 →
// C0001M01.XML).
//
// It exists because exiftool alone is not enough for these clips: the MP4
// carries a QuickTime CreateDate in UTC with no offset, so the mtime/UTC
// fallback files a 17:35 local recording under the wrong day. The sidecar's
// <CreationDate> is the camera's own local wall-clock time with an offset,
// which is exactly what originals/YYYY/MM/DD needs. The sidecar also names
// the body and the lens, neither of which the Sony MP4 container exposes.
//
// Only the fields the pipeline uses are parsed; everything else in the
// document (LtcChangeTable, KlvPacketTable, AcquisitionRecord…) is ignored by
// encoding/xml without any explicit skipping.
package nrtmeta

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thevedantmodi/framelog/core/config"
)

// Meta is the parsed subset of a NonRealTimeMeta document.
// Every field is optional — a sidecar that omits one is not an error, callers
// fall back to whatever exiftool gave them.
type Meta struct {
	CreationDate time.Time // zero when absent or unparseable
	CameraModel  string    // Device modelName, e.g. "ILCE-7CM2"
	LensModel    string    // Lens modelName, e.g. "SAMYANG AF 35mm F1.8"
}

// nonRealTimeMeta mirrors the document shape. The namespace is deliberately
// not pinned in the struct tags: Sony bumps the ver.X.YY in the xmlns between
// firmware revisions, and encoding/xml would then silently stop matching.
type nonRealTimeMeta struct {
	CreationDate struct {
		Value string `xml:"value,attr"`
	} `xml:"CreationDate"`
	Device struct {
		ModelName string `xml:"modelName,attr"`
	} `xml:"Device"`
	Lens struct {
		ModelName string `xml:"modelName,attr"`
	} `xml:"Lens"`
}

// Parse reads and parses the sidecar at path.
func Parse(path string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, fmt.Errorf("nrtmeta: read %s: %w", path, err)
	}
	return ParseBytes(data)
}

// ParseBytes parses a NonRealTimeMeta document from memory. Split from Parse
// so tests exercise the XML handling without touching the filesystem.
func ParseBytes(data []byte) (Meta, error) {
	var doc nonRealTimeMeta
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Meta{}, fmt.Errorf("nrtmeta: parse XML: %w", err)
	}

	var m Meta
	if v := doc.CreationDate.Value; v != "" {
		// Sony writes RFC3339 with an offset ("2026-07-26T17:35:25-07:00").
		// Older bodies omit the offset, in which case the timestamp is already
		// camera-local and is read as such.
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			m.CreationDate = t
		} else if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, time.Local); err == nil {
			m.CreationDate = t
		}
	}
	m.CameraModel = doc.Device.ModelName
	m.LensModel = doc.Lens.ModelName
	return m, nil
}

// FindSidecar returns the path to the NonRealTimeMeta sidecar for clipPath, or
// "" when there is none. The camera names it "<stem>M01.XML"; the M01 counter
// increments for clips split across a 4GB boundary, and some tools drop the
// suffix entirely, so any .xml file in the same directory whose stem starts
// with the clip's stem qualifies. Matching is case-insensitive throughout
// because a card formatted in-camera stores names uppercase while a copy made
// on macOS may not preserve that.
func FindSidecar(clipPath string) string {
	dir := filepath.Dir(clipPath)
	base := filepath.Base(clipPath)
	stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	if stem == "" {
		return ""
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	// Prefer the exact "<stem>M01.XML" spelling over any other match so a clip
	// never picks up a longer neighbour's sidecar by accident.
	var fallback string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.ToLower(filepath.Ext(name)) != config.NRTMetaExtension {
			continue
		}
		sidecarStem := strings.TrimSuffix(lower, config.NRTMetaExtension)
		if sidecarStem == stem+"m01" {
			return filepath.Join(dir, name)
		}
		if sidecarStem == stem && fallback == "" {
			fallback = filepath.Join(dir, name)
		}
	}
	return fallback
}

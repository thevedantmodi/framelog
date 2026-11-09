package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// MediaType is one camera file type Framelog knows about. Label is what the
// menu bar shows next to the extension.
type MediaType struct {
	Ext   string
	Label string
}

// KnownMediaTypes is every camera file type Framelog recognises, in the order
// the menu bar lists them. All of them are imported by default; the user can
// switch any of them off (set_import_types). The distinction matters for card
// verification:
//   - a type that is switched on is imported and must be verified;
//   - a known type the user switched off is "ignored" — listed on the card
//     report, but it does not block "safe to format" (the user chose it);
//   - any other file in DCIM/ or the clip folder is "unhandled" and blocks,
//     because formatting would silently destroy something nobody chose to drop.
//
// That is also why every known type defaults to on: a type that is off by
// default would be dropped without the user ever deciding to drop it.
var KnownMediaTypes = []MediaType{
	{".arw", "Sony RAW"},
	// Sony names HEIF stills .HIF (α7C II, α7 IV, α1 …). Same container as
	// .heic; exiftool reads it and Lightroom imports it.
	{".hif", "Sony HEIF"},
	{".cr3", "Canon RAW"},
	{".cr2", "Canon RAW (older bodies)"},
	{".nef", "Nikon RAW"},
	{".raf", "Fujifilm RAW"},
	{".orf", "OM System / Olympus RAW"},
	{".rw2", "Panasonic RAW"},
	{".dng", "DNG RAW"},
	{".heic", "HEIF"},
	{".jpg", "JPEG"},
	{".jpeg", "JPEG"},
	{".mp4", "MP4 video"},
	{".mov", "QuickTime video"},
	{".wav", "WAV audio"},
}

// outgestOnlyExtensions are formats outgest files but ingest never imports.
// A PNG export must be filed into processed/YYYY/MM but must never enter
// originals/ and git. Kept narrow on purpose: outgest sets
// status="published", so Photoshop working files (.psb/.psd) and archival
// intermediates (.jxl), which share the export's hash8 prefix, stay out.
var outgestOnlyExtensions = map[string]bool{
	".png": true,
}

// neverImportable can't be switched on: .xml is the Sony clip sidecar
// (metadata for a clip, read by ingest, never a library item), .xmp is
// Framelog's own sidecar, .png is export-only (see above).
var neverImportable = map[string]bool{
	NRTMetaExtension: true,
	".xmp":           true,
	".png":           true,
}

var knownExt = func() map[string]bool {
	m := make(map[string]bool, len(KnownMediaTypes))
	for _, t := range KnownMediaTypes {
		m[t.Ext] = true
	}
	return m
}()

// importExts holds the current import set. Each published map is immutable
// once stored, so readers can index it without a lock.
var importExts atomic.Pointer[map[string]bool]

func init() { resetImportExtensions() }

func resetImportExtensions() {
	m := make(map[string]bool, len(KnownMediaTypes))
	for _, t := range KnownMediaTypes {
		m[t.Ext] = true
	}
	importExts.Store(&m)
}

// DefaultImportExtensions returns the import set used when the user has not
// chosen one: every known type, sorted.
func DefaultImportExtensions() []string {
	out := make([]string, 0, len(KnownMediaTypes))
	for _, t := range KnownMediaTypes {
		out = append(out, t.Ext)
	}
	sort.Strings(out)
	return out
}

// IsImportable reports whether files with this extension (any case, with
// the dot) are imported into the library.
func IsImportable(ext string) bool {
	return (*importExts.Load())[strings.ToLower(ext)]
}

// IsKnownMediaType reports whether ext is in KnownMediaTypes, whether or not
// it is currently switched on.
func IsKnownMediaType(ext string) bool { return knownExt[strings.ToLower(ext)] }

// IsOutgestExtension reports whether outgest files exports with this
// extension into processed/YYYY/MM. It is every known camera type, every
// type the user imports, and the export-only types — deliberately
// independent of which types are switched off for import, so switching off
// JPEG import never stops Lightroom's JPEG exports being filed.
func IsOutgestExtension(ext string) bool {
	ext = strings.ToLower(ext)
	return knownExt[ext] || outgestOnlyExtensions[ext] || IsImportable(ext)
}

// ImportExtensions returns the current import set, sorted.
func ImportExtensions() []string {
	m := *importExts.Load()
	out := make([]string, 0, len(m))
	for ext := range m {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

var extRE = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

// NormalizeExtension lower-cases ext and adds a leading dot if missing, so
// "ARW", ".ARW" and "arw" are the same type.
func NormalizeExtension(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// ErrInvalidFileType wraps every validation failure from
// ValidateImportExtensions / SetImportExtensions.
var ErrInvalidFileType = errors.New("invalid file type")

// ValidateImportExtensions normalises and checks a proposed import set
// without applying it, returning it sorted and de-duplicated. Each
// extension must be a dot plus 1–10 letters/digits, must not be one of the
// never-importable types, and at least one is required (an empty set would
// make every card "nothing to import").
func ValidateImportExtensions(exts []string) ([]string, error) {
	m := make(map[string]bool, len(exts))
	for _, raw := range exts {
		ext := NormalizeExtension(raw)
		if !extRE.MatchString(ext) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidFileType, raw)
		}
		if neverImportable[ext] {
			return nil, fmt.Errorf("%w: %s files can't be imported", ErrInvalidFileType, ext)
		}
		m[ext] = true
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%w: at least one file type must be imported", ErrInvalidFileType)
	}
	out := make([]string, 0, len(m))
	for ext := range m {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out, nil
}

// SetImportExtensions replaces the import set; nil restores the defaults.
// See ValidateImportExtensions for the rules. A rejected set leaves the
// current one untouched.
func SetImportExtensions(exts []string) error {
	if exts == nil {
		resetImportExtensions()
		return nil
	}
	valid, err := ValidateImportExtensions(exts)
	if err != nil {
		return err
	}
	m := make(map[string]bool, len(valid))
	for _, ext := range valid {
		m[ext] = true
	}
	importExts.Store(&m)
	return nil
}

// FileTypeSetting is one row of the file-type list shown to the user.
type FileTypeSetting struct {
	Ext     string `json:"ext"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Custom  bool   `json:"custom"` // added by the user, not in KnownMediaTypes
}

// FileTypeSettings lists every known type (on or off) followed by any custom
// types the user added, sorted.
func FileTypeSettings() []FileTypeSetting {
	m := *importExts.Load()
	out := make([]FileTypeSetting, 0, len(KnownMediaTypes)+len(m))
	for _, t := range KnownMediaTypes {
		out = append(out, FileTypeSetting{Ext: t.Ext, Label: t.Label, Enabled: m[t.Ext]})
	}
	var custom []string
	for ext := range m {
		if !knownExt[ext] {
			custom = append(custom, ext)
		}
	}
	sort.Strings(custom)
	for _, ext := range custom {
		out = append(out, FileTypeSetting{Ext: ext, Label: "Custom", Enabled: true, Custom: true})
	}
	return out
}

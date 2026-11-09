// Package config is the single source of truth for paths and constants used
// across the core. Every other package imports from here — nothing is ever
// hardcoded a second time elsewhere (this is the rule the Python version
// didn't quite keep, e.g. the launchd plists hardcoding a username).
package config

import (
	"os"
	"path/filepath"
)

var home, _ = os.UserHomeDir()

var (
	Inbox     = filepath.Join(home, "Photos", "inbox")
	Originals = filepath.Join(home, "Photos", "originals")
	Processed = filepath.Join(home, "Photos", "processed")
	DBPath    = filepath.Join(home, "Photos", "catalog.db")
	LogFile   = filepath.Join(home, "Photos", "framelog.log")

	// IngestTrigger is the v1 IPC mechanism (FL-301). Swift touches this file;
	// the core watches/polls for it. Superseded by the socket in FL-302 but
	// kept as the fallback transport.
	IngestTrigger = filepath.Join(home, "Photos", ".ingest_trigger")

	// OutgestTrigger is the v1 IPC mechanism for outgest — the contract extension
	// documented in PROTOCOL.md §2 (added from Swift side in FL-404, Go-side polling
	// implemented in FL-301). Same pattern as IngestTrigger.
	OutgestTrigger = filepath.Join(home, "Photos", ".outgest_trigger")

	// SocketPath is the v2 IPC mechanism (FL-302).
	SocketPath = filepath.Join(home, "Library", "Application Support", "Framelog", "framelog.sock")

	// CrashLogPath is where launchd's StandardOutPath/StandardErrorPath point (FL-303).
	// Deliberately separate from LogFile: logging.Logger already writes directly to LogFile
	// AND to stdout, so pointing launchd's output capture at LogFile would duplicate every
	// structured line. CrashLogPath catches only things that bypass logging.Logger entirely:
	// uncaught panic stack traces (Go runtime writes to real stderr, not through Logger),
	// or failures before logging.New even succeeds.
	CrashLogPath = filepath.Join(home, "Library", "Logs", "Framelog", "crash.log")

	// BackupPath is where the deduped originals/ library gets synced after a
	// successful ingest (FL-207). Empty/unset means backup is disabled.
	BackupPath = os.Getenv("FRAMELOG_BACKUP_PATH")

	// ReportsDir holds one plain-text receipt per verified SD card
	// (cardverify). Durable, human-readable proof of what was on the card
	// and where every file ended up — the thing you check before formatting.
	ReportsDir = filepath.Join(home, "Photos", "reports")

	// UserConfigPath is the persisted JSON file written by set_backup_path IPC.
	// Takes precedence over FRAMELOG_BACKUP_PATH at daemon startup.
	UserConfigPath = filepath.Join(home, "Library", "Application Support", "Framelog", "framelog_config.json")
)

// DuplicatesDirName is the subdirectory of inbox/ where ingest parks source
// files whose hash already exists in the catalog (PROTOCOL.md §8). Keeping
// them inside inbox/ makes them easy to find and review; the ingest walk
// skips this directory so they are never re-scanned.
const DuplicatesDirName = "duplicates"

// FailedDirName is the subdirectory of inbox/ where ingest quarantines source
// files that keep failing to import (zero-byte files, exiftool errors) so a
// poison file cannot retry forever on every run (PROTOCOL.md §8). Like
// DuplicatesDirName, the ingest walk skips it.
const FailedDirName = "failed"

// DCIMDirName is the standard camera image directory on a card. Stills (and,
// on most cameras, video) live under it in numbered subfolders.
const DCIMDirName = "DCIM"

// ClipDirRelPath is where Sony XAVC-S/XAVC-HS cameras write their video
// clips — outside DCIM entirely. Each C####.MP4 there is accompanied by a
// C####M01.XML NonRealTimeMeta sidecar carrying the real capture timestamp,
// camera body, and lens; see core/nrtmeta.
var ClipDirRelPath = filepath.Join("PRIVATE", "M4ROOT", "CLIP")

// NRTMetaExtension is the extension of the Sony NonRealTimeMeta sidecar.
// Deliberately never importable (see filetypes.go): the sidecar is metadata for a
// clip, never an importable file in its own right. Ingest reads it, copies
// it next to the imported clip (where .gitignore keeps it untracked), and
// deletes the inbox copy along with the clip.
const NRTMetaExtension = ".xml"

// The import / export file-type sets live in filetypes.go: the import set
// is user-configurable at runtime (set_import_types), so it is behind
// accessors rather than a plain map.

// EmbeddedXMPExtensions is the set of formats that store XMP metadata embedded
// inside the file rather than in a separate sidecar. Writing a .xmp sidecar
// next to these files causes Lightroom to read the sidecar and ignore the
// embedded data, hiding develop edits. The xmpwatcher extracts XMP from these
// files after Lightroom edits them and writes it to a sidecar for git tracking.
var EmbeddedXMPExtensions = map[string]bool{
	".dng": true,
}

const DebounceSeconds = 10

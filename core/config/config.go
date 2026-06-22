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

	// SocketPath is the v2 IPC mechanism (FL-302).
	SocketPath = filepath.Join(home, "Library", "Application Support", "Framelog", "framelog.sock")

	// BackupPath is where the deduped originals/ library gets synced after a
	// successful ingest (FL-207). Empty/unset means backup is disabled.
	BackupPath = os.Getenv("FRAMELOG_BACKUP_PATH")
)

// SupportedExtensions mirrors config.py's SUPPORTED_EXTENSIONS. Decide here,
// in one place, which RAW/video formats this install cares about.
var SupportedExtensions = map[string]bool{
	".raf":  true,
	".cr3":  true,
	".dng":  true,
	".heic": true,
	".jpg":  true,
	".jpeg": true,
	".mp4":  true,
	".mov":  true,
}

const DebounceSeconds = 10

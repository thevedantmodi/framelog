// Package logging provides the single structured logger used across all core
// goroutines. Format is frozen in PROTOCOL.md §5:
//
//	2006-01-02 15:04:05 [PREFIX] message
//
// One Logger instance is shared across the process. All writes are serialised
// with a mutex (Phase 2 runs ingest, XMP watcher, and outgest watcher
// concurrently) and each write is followed by an fsync so a crash immediately
// after a Log call cannot lose that line. This is the gap the Python version
// had: ingest.py used bare print() while outgest.py used log() without
// flush=True, so lines could be lost on abnormal exit.
package logging

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Prefix is the bracketed tag that identifies which pipeline stage emitted a
// log line. The set is closed — adding a new value means updating PROTOCOL.md
// §5 in the same commit, not inventing an ad hoc string.
type Prefix string

const (
	PrefixIngest  Prefix = "INGEST"
	PrefixOutgest Prefix = "OUTGEST"
	PrefixXMP     Prefix = "XMP"
	PrefixBackup  Prefix = "BACKUP"
	PrefixGit     Prefix = "GIT"
	PrefixCore    Prefix = "CORE"
)

// timestampFormat matches the PROTOCOL.md §5 example exactly.
const timestampFormat = "2006-01-02 15:04:05"

// Logger writes timestamped, prefixed lines to a file and to stdout. All
// methods are safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	file *os.File
}

// New opens path in append+create mode and returns a Logger backed by that
// file. The caller must call Close when done. Path is taken as a parameter
// (never read from config.LogFile directly) to match the pattern established
// by db.Open and every other leaf package: the caller injects the path.
func New(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logging.New: %w", err)
	}
	return &Logger{file: f}, nil
}

// Log writes one line — "TIMESTAMP [PREFIX] message\n" — to both stdout and
// the backing file, then calls Sync on the file. The Sync ensures the line is
// on disk before Log returns; without it a crash could silently drop the line
// even though the write syscall succeeded (the line would still be in the OS
// page cache).
func (l *Logger) Log(prefix Prefix, message string) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format(timestampFormat), prefix, message)

	l.mu.Lock()
	defer l.mu.Unlock()

	// Write to stdout first so the line is visible even if the file write
	// fails; ignore the error — stdout failure is not actionable here.
	io.WriteString(os.Stdout, line) //nolint:errcheck

	if _, err := fmt.Fprint(l.file, line); err != nil {
		// Log to stderr so we don't silently swallow the failure, but don't
		// panic — a logging failure should not crash the pipeline.
		fmt.Fprintf(os.Stderr, "logging: write failed: %v\n", err)
		return
	}
	if err := l.file.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "logging: sync failed: %v\n", err)
	}
}

// Close flushes and closes the backing file. Safe to call exactly once.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("logging.Close sync: %w", err)
	}
	return l.file.Close()
}

// Package rclonerun runs `rclone copy` and streams its --use-json-log output
// so callers can report per-file progress instead of blocking until the
// whole invocation completes. It is shared by core/sdcard (DCIM → inbox) and
// core/backup (originals → backup drive) — both shell out to rclone and both
// need the same JSON-log parsing, so the logic lives here once instead of
// being duplicated per caller.
package rclonerun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// logEntry is one line of rclone's --use-json-log output.
type logEntry struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Object string `json:"object"`
}

// FileError is one per-file failure rclone reported at "error" level.
type FileError struct {
	Object string // path relative to the copy source, as rclone logs it
	Msg    string
}

// Result is everything Run learned from one rclone invocation.
type Result struct {
	Copied int
	// Errors lists per-file failures in the order rclone reported them.
	// rclone retries a failing transfer (default --retries 3), so one bad
	// file can appear more than once; callers wanting a count of distinct
	// files should use FailedObjects.
	Errors []FileError
}

// FailedObjects returns the distinct objects that had at least one error,
// in first-seen order. Errors without an object (e.g. "Attempt 1/3 failed
// with 2 errors") are summary lines and are left out.
func (r Result) FailedObjects() []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range r.Errors {
		if e.Object == "" || seen[e.Object] {
			continue
		}
		seen[e.Object] = true
		out = append(out, e.Object)
	}
	return out
}

// Copy runs `rclonePath args...`, appending --use-json-log and -v so the
// per-file progress parsing below has output to work with. args should
// already contain "copy", the source and destination, and any filter flags
// (--include, --ignore-existing, etc.) the caller needs.
//
// onCopy, if non-nil, is invoked after each file rclone reports copied, with
// the file's base name and the running copied-so-far count. Returns the
// final count and an error wrapping rclone's stderr output on non-zero exit.
func Copy(rclonePath string, args []string, onCopy func(filename string, n int)) (int, error) {
	res, err := Run(rclonePath, args, onCopy)
	return res.Copied, err
}

// Run is Copy with the full Result: the copied count plus every per-file
// error rclone logged. Callers that report failures to the user (backup,
// card copy) use this so they can say *which* files failed and why instead
// of only "exit status 1".
func Run(rclonePath string, args []string, onCopy func(filename string, n int)) (Result, error) {
	args = append(args, "--use-json-log", "-v")

	var res Result
	cmd := exec.Command(rclonePath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return res, fmt.Errorf("rclone copy: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("rclone copy: %w", err)
	}

	var stderrBuf bytes.Buffer
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Bytes()
		stderrBuf.Write(line)
		stderrBuf.WriteByte('\n')

		var entry logEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // non-JSON noise line
		}
		// "Copied (new)" for a normal transfer, "Copied (server-side copy)"
		// when src/dst share a filesystem and rclone uses a reflink/clonefile
		// instead — match both, but not "Copied (replaced existing)" since
		// that isn't a fresh copy.
		if entry.Level == "error" || entry.Level == "critical" {
			res.Errors = append(res.Errors, FileError{Object: entry.Object, Msg: entry.Msg})
			continue
		}
		if strings.HasPrefix(entry.Msg, "Copied (") && !strings.Contains(entry.Msg, "replaced existing") {
			res.Copied++
			if onCopy != nil {
				onCopy(filepath.Base(entry.Object), res.Copied)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return res, fmt.Errorf("rclone copy: %w; stderr: %s",
			err, bytes.TrimSpace(stderrBuf.Bytes()))
	}
	return res, nil
}

// Explain turns an rclone failure into one sentence a photographer can act
// on. rclone's raw stderr is a wall of JSON; the menu bar and the card
// report need "Backup drive is full", not "exit status 1". Matching is on
// the OS error strings Go's os package produces on macOS, which rclone
// passes through verbatim. Unknown failures fall back to the first per-file
// error message (or the raw error), trimmed to something that fits a menu.
func Explain(res Result, err error) string {
	if err == nil && len(res.Errors) == 0 {
		return ""
	}
	var hay strings.Builder
	if err != nil {
		hay.WriteString(err.Error())
	}
	for _, e := range res.Errors {
		hay.WriteString("\n")
		hay.WriteString(e.Msg)
	}
	text := strings.ToLower(hay.String())

	switch {
	case strings.Contains(text, "no space left on device"):
		return "Backup drive is full"
	case strings.Contains(text, "read-only file system"):
		return "Drive is read-only (NTFS-formatted drives are read-only on macOS)"
	case strings.Contains(text, "operation not permitted"), strings.Contains(text, "permission denied"):
		return "Permission denied — grant framelogd Full Disk Access in System Settings › Privacy & Security"
	case strings.Contains(text, "input/output error"):
		return "Drive I/O error — the drive may be failing or was unplugged mid-copy"
	case strings.Contains(text, "no such file or directory"), strings.Contains(text, "directory not found"):
		return "Drive disappeared during the copy — was it ejected?"
	case strings.Contains(text, "file name too long"):
		return "A file name is too long for the destination drive's format"
	}

	msg := ""
	if len(res.Errors) > 0 {
		msg = res.Errors[0].Msg
		if o := res.Errors[0].Object; o != "" {
			msg = o + ": " + msg
		}
	} else if err != nil {
		msg = err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 160
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return msg
}

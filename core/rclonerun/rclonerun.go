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
	Msg    string `json:"msg"`
	Object string `json:"object"`
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
	args = append(args, "--use-json-log", "-v")

	cmd := exec.Command(rclonePath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, fmt.Errorf("rclone copy: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("rclone copy: %w", err)
	}

	var count int
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
		if strings.HasPrefix(entry.Msg, "Copied (") && !strings.Contains(entry.Msg, "replaced existing") {
			count++
			if onCopy != nil {
				onCopy(filepath.Base(entry.Object), count)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return count, fmt.Errorf("rclone copy: %w; stderr: %s",
			err, bytes.TrimSpace(stderrBuf.Bytes()))
	}
	return count, nil
}

package rclonerun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

// TestCopy_ParsesJSONAndInvokesCallback verifies both "Copied (new)" and
// "Copied (server-side copy)" messages count and fire onCopy, in order, with
// the running count and object base name.
func TestCopy_ParsesJSONAndInvokesCallback(t *testing.T) {
	rclone := writeFakeBin(t, t.TempDir(), "rclone", `cat >&2 <<'EOF'
{"level":"info","msg":"Copied (new)","object":"100CANON/a.jpg"}
{"level":"info","msg":"Copied (server-side copy)","object":"100CANON/b.jpg"}
EOF`)

	var calls []string
	n, err := Copy(rclone, []string{"copy", "src", "dst"}, func(filename string, i int) {
		calls = append(calls, filename)
		if i != len(calls) {
			t.Errorf("callback count = %d, want %d", i, len(calls))
		}
	})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	want := []string{"a.jpg", "b.jpg"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

// TestCopy_IgnoresReplacedExistingAndNoise verifies "Copied (replaced
// existing)" is not counted, and non-JSON log lines are skipped rather than
// aborting the parse.
func TestCopy_IgnoresReplacedExistingAndNoise(t *testing.T) {
	rclone := writeFakeBin(t, t.TempDir(), "rclone", `cat >&2 <<'EOF'
not json, just rclone startup noise
{"level":"info","msg":"Copied (replaced existing)","object":"100CANON/old.jpg"}
{"level":"info","msg":"Copied (new)","object":"100CANON/new.jpg"}
EOF`)

	n, err := Copy(rclone, []string{"copy", "src", "dst"}, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (replaced-existing and noise line should not count)", n)
	}
}

// TestCopy_ErrorWrapsStderr asserts a non-zero rclone exit returns an error
// that includes the stderr text, even when that text isn't valid JSON.
func TestCopy_ErrorWrapsStderr(t *testing.T) {
	rclone := writeFakeBin(t, t.TempDir(), "rclone",
		`echo "permission denied" >&2; exit 1`)

	_, err := Copy(rclone, []string{"copy", "src", "dst"}, nil)
	if err == nil {
		t.Fatal("Copy returned nil error for failing rclone")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not contain stderr text \"permission denied\"", err.Error())
	}
}

// TestCopy_NilCallbackDoesNotPanic verifies onCopy=nil is a valid no-op.
func TestCopy_NilCallbackDoesNotPanic(t *testing.T) {
	rclone := writeFakeBin(t, t.TempDir(), "rclone",
		`echo '{"level":"info","msg":"Copied (new)","object":"a.jpg"}' >&2`)

	n, err := Copy(rclone, []string{"copy", "src", "dst"}, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

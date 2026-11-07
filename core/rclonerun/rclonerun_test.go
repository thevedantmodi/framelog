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

// TestRun_CollectsPerFileErrors verifies error-level JSON lines are captured
// (with their object) and do not count as copies, and FailedObjects dedups
// rclone's retry repeats while dropping object-less summary lines.
func TestRun_CollectsPerFileErrors(t *testing.T) {
	rclone := writeFakeBin(t, t.TempDir(), "rclone", `cat >&2 <<'EOF2'
{"level":"info","msg":"Copied (new)","object":"2026/11/07/a.arw"}
{"level":"error","msg":"Failed to copy: write /Volumes/B/originals/2026/11/07/b.arw: no space left on device","object":"2026/11/07/b.arw"}
{"level":"error","msg":"Failed to copy: write: no space left on device","object":"2026/11/07/b.arw"}
{"level":"error","msg":"Attempt 1/3 failed with 1 errors","object":""}
EOF2
exit 1`)

	res, err := Run(rclone, []string{"copy", "src", "dst"}, nil)
	if err == nil {
		t.Fatal("Run returned nil error for non-zero exit")
	}
	if res.Copied != 1 {
		t.Errorf("Copied = %d, want 1", res.Copied)
	}
	if len(res.Errors) != 3 {
		t.Fatalf("Errors = %d, want 3: %+v", len(res.Errors), res.Errors)
	}
	got := res.FailedObjects()
	if len(got) != 1 || got[0] != "2026/11/07/b.arw" {
		t.Errorf("FailedObjects = %v, want [2026/11/07/b.arw]", got)
	}
	if msg := Explain(res, err); msg != "Backup drive is full" {
		t.Errorf("Explain = %q, want %q", msg, "Backup drive is full")
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		err  error
		want string
	}{
		{"success", Result{Copied: 3}, nil, ""},
		{"readonly", Result{Errors: []FileError{{Object: "a", Msg: "open: read-only file system"}}}, nil,
			"Drive is read-only (NTFS-formatted drives are read-only on macOS)"},
		{"perm", Result{}, errString("rclone copy: exit status 1; stderr: operation not permitted"),
			"Permission denied — grant framelogd Full Disk Access in System Settings › Privacy & Security"},
		{"io", Result{Errors: []FileError{{Msg: "read: input/output error"}}}, nil,
			"Drive I/O error — the drive may be failing or was unplugged mid-copy"},
		{"gone", Result{}, errString("directory not found"), "Drive disappeared during the copy — was it ejected?"},
		{"fallback uses first file error", Result{Errors: []FileError{{Object: "x.arw", Msg: "weird thing"}}}, nil,
			"x.arw: weird thing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Explain(c.res, c.err); got != c.want {
				t.Errorf("Explain = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExplain_TruncatesLongFallback(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := Explain(Result{}, errString(long))
	if len(got) > 170 {
		t.Errorf("Explain returned %d bytes, want truncated", len(got))
	}
}

type errString string

func (e errString) Error() string { return string(e) }

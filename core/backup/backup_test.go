package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBin creates dir/<name> as an executable shell script running body.
func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

// TestSync_BackupPathMissing asserts Sync returns (false, nil) without ever
// invoking rclone when backupPath does not exist. An unplugged drive must not
// fail the ingest batch that triggered the call.
func TestSync_BackupPathMissing(t *testing.T) {
	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "rclone-called")

	// Fake rclone writes a sentinel file when invoked; if Sync short-circuits
	// before calling it, the file must remain absent.
	rclone := writeFakeBin(t, binDir, "rclone",
		"touch "+sentinel)

	synced, err := Sync(rclone, t.TempDir(), "/nonexistent/backup/drive/path")
	if err != nil {
		t.Fatalf("Sync returned error for missing path: %v", err)
	}
	if synced {
		t.Error("Sync returned synced=true for missing backupPath")
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("rclone was invoked despite backupPath not existing (Sync should short-circuit)")
	}
}

// TestSync_Success verifies the args Sync passes to rclone are exactly
// ["copy", originalsPath, backupPath+"/originals"].
func TestSync_Success(t *testing.T) {
	binDir := t.TempDir()
	originals := t.TempDir()
	backupPath := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "rclone-args")

	// Fake rclone records its arguments and exits 0.
	rclone := writeFakeBin(t, binDir, "rclone",
		`echo "$@" > `+argsFile)

	synced, err := Sync(rclone, originals, backupPath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !synced {
		t.Error("Sync returned synced=false for existing backupPath")
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("rclone args file not written: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	wantParts := []string{"copy", originals, filepath.Join(backupPath, "originals")}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Errorf("rclone args %q does not contain %q", got, part)
		}
	}
}

// TestSync_RcloneFailure asserts that a non-zero rclone exit returns
// synced=false and a non-nil error that includes the stderr text.
func TestSync_RcloneFailure(t *testing.T) {
	binDir := t.TempDir()
	backupPath := t.TempDir()

	rclone := writeFakeBin(t, binDir, "rclone",
		`echo "permission denied" >&2; exit 1`)

	synced, err := Sync(rclone, t.TempDir(), backupPath)
	if err == nil {
		t.Fatal("Sync returned nil error for failing rclone")
	}
	if synced {
		t.Error("Sync returned synced=true for failing rclone")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not contain stderr text \"permission denied\"", err.Error())
	}
}

// TestFindRclone_LookPathFallback forces the candidate-list check to fail and
// asserts FindRclone resolves via exec.LookPath when a fake rclone is on PATH.
func TestFindRclone_LookPathFallback(t *testing.T) {
	orig := rcloneCandidates
	rcloneCandidates = []string{"/nonexistent/rclone-a", "/nonexistent/rclone-b"}
	t.Cleanup(func() { rcloneCandidates = orig })

	binDir := t.TempDir()
	writeFakeBin(t, binDir, "rclone", `exit 0`)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	p, err := FindRclone()
	if err != nil {
		t.Fatalf("FindRclone: %v", err)
	}
	if p == "" {
		t.Fatal("FindRclone returned empty path")
	}
}

func TestFindRclone_NotFound(t *testing.T) {
	orig := rcloneCandidates
	rcloneCandidates = []string{"/nonexistent/rclone-a"}
	t.Cleanup(func() { rcloneCandidates = orig })

	t.Setenv("PATH", t.TempDir())
	_, err := FindRclone()
	if err == nil {
		t.Fatal("FindRclone returned nil error when binary absent")
	}
}

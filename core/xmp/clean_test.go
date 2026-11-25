package xmp

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		p := filepath.Join(root, r)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestClean_RemovesSidecarsKeepsPhotosAndGit(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root,
		"2026/06/22/a.raf", "2026/06/22/a.xmp",
		"2026/06/23/b.dng", "2026/06/23/b.XMP",
		"2026/06/23/c.xml",
		".git/objects/keep.xmp", ".gitignore",
	)

	res, err := Clean(root, false)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(res.Found) != 2 || res.Removed != 2 {
		t.Fatalf("found %d removed %d, want 2/2: %v", len(res.Found), res.Removed, res.Found)
	}
	for _, gone := range []string{"2026/06/22/a.xmp", "2026/06/23/b.XMP"} {
		if exists(filepath.Join(root, gone)) {
			t.Errorf("%s still exists", gone)
		}
	}
	for _, kept := range []string{"2026/06/22/a.raf", "2026/06/23/b.dng", "2026/06/23/c.xml", ".git/objects/keep.xmp", ".gitignore"} {
		if !exists(filepath.Join(root, kept)) {
			t.Errorf("%s was removed, must be kept", kept)
		}
	}
}

func TestClean_DryRunDeletesNothing(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "2026/06/22/a.raf", "2026/06/22/a.xmp")

	res, err := Clean(root, true)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(res.Found) != 1 || res.Removed != 0 {
		t.Fatalf("found %d removed %d, want 1/0", len(res.Found), res.Removed)
	}
	if !exists(filepath.Join(root, "2026/06/22/a.xmp")) {
		t.Error("dry run deleted the sidecar")
	}
}

func TestClean_DoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, outside, "elsewhere.xmp")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "elsewhere.xmp"), filepath.Join(root, "file.xmp")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	res, err := Clean(root, false)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(res.Found) != 0 {
		t.Errorf("found %v, want nothing (symlinks must not be followed or removed)", res.Found)
	}
	if !exists(filepath.Join(outside, "elsewhere.xmp")) {
		t.Error("file outside root was deleted")
	}
}

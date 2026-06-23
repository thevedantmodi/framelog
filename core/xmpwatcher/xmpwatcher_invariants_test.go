// xmpwatcher_invariants_test.go documents every cross-cutting guarantee the
// xmpwatcher package makes. Each test is named after the invariant it enforces.
package xmpwatcher

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestInvariant_GitDirExcluded verifies the FL-204 guarantee: the xmpwatcher
// never adds .git or any of its subdirectories to the fsnotify watch list.
// Without this exclusion, every git commit the watcher itself makes would
// generate watch events, trigger a debounce, and schedule another commit —
// an infinite loop of empty commits that corrupts the originals/ repo history.
func TestInvariant_GitDirExcluded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires fsnotify + git")
	}
	git := findGit(t)
	originals := t.TempDir()
	setupRepo(t, git, originals)

	var commitCalls atomic.Int32
	w, _, _ := newWatcher(t, git, originals, 100*time.Millisecond)
	w.onRunCommit = func() { commitCalls.Add(1) }

	go w.Run()
	time.Sleep(200 * time.Millisecond) // wait for startup walk

	// Create a .xmp file INSIDE .git — if .git were watched, this would fire.
	gitObj := filepath.Join(originals, ".git", "objects", "ab")
	if err := os.MkdirAll(gitObj, 0o755); err != nil {
		t.Fatalf("mkdir .git/objects/ab: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitObj, "fake.xmp"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write under .git: %v", err)
	}

	// Wait past the debounce to give the watcher a chance to (incorrectly) fire.
	time.Sleep(500 * time.Millisecond)
	w.Stop()

	if n := commitCalls.Load(); n != 0 {
		t.Errorf("runCommit was called %d time(s) by .git writes, want 0", n)
	}
	// Git log must still show only the initial commit.
	if n := gitLogCount(t, git, originals); n != 1 {
		t.Errorf("git log count = %d after .git writes, want 1 (no spurious commits)", n)
	}
}

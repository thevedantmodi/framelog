package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a git repo in dir, sets a local user identity so commits
// don't require global git config, and returns the git binary path.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	git, err := FindGit()
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@framelog.test")
	run("config", "user.name", "Framelog Test")
	return git
}

// writeFile creates or overwrites name inside dir with content.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}

func TestCommit_NothingToCommit(t *testing.T) {
	dir := t.TempDir()
	git := initRepo(t, dir)

	committed, err := Commit(git, dir, "should not appear")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if committed {
		t.Error("committed=true on empty repo, want false")
	}

	// git log on an empty repo exits non-zero; no commits means no output.
	out, _ := exec.Command(git, "-C", dir, "log", "--oneline").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("git log non-empty after no-op commit: %q", out)
	}
}

func TestCommit_WithChanges(t *testing.T) {
	dir := t.TempDir()
	git := initRepo(t, dir)

	writeFile(t, dir, "photo.raf", "fake raw bytes")

	committed, err := Commit(git, dir, "ingest 2026-06-22")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !committed {
		t.Error("committed=false after adding a file, want true")
	}

	out, err := exec.Command(git, "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "ingest 2026-06-22" {
		t.Errorf("commit subject = %q, want %q", got, "ingest 2026-06-22")
	}
}

// setupRepoWithRemote creates a repo in dir with an initial commit, and a bare
// repo in bareDir configured as its "origin" remote. Returns the git path.
func setupRepoWithRemote(t *testing.T) (git, repoDir, bareDir string) {
	t.Helper()
	repoDir = t.TempDir()
	bareDir = t.TempDir()

	git = initRepo(t, repoDir)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Initialise bare repo.
	run(bareDir, "init", "--bare")

	// Make an initial commit in the working repo so the branch exists.
	writeFile(t, repoDir, "init.txt", "init")
	run(repoDir, "add", "-A")
	run(repoDir, "commit", "-m", "init")

	// Wire up remote and push the initial commit so the remote branch exists.
	run(repoDir, "remote", "add", "origin", bareDir)
	run(repoDir, "push", "-u", "origin", "HEAD")

	return git, repoDir, bareDir
}

func TestPush_NotOnAC(t *testing.T) {
	git, repoDir, bareDir := setupRepoWithRemote(t)

	// Add a new commit that we expect NOT to be pushed.
	writeFile(t, repoDir, "photo2.raf", "more bytes")
	if _, err := Commit(git, repoDir, "second commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pushed, err := Push(git, repoDir, false)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed {
		t.Error("pushed=true when onACPower=false, want false")
	}

	// Bare repo should still only have the initial commit.
	out, err := exec.Command(git, "-C", bareDir, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log bare: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		t.Errorf("bare repo has %d commits after skipped push, want 1: %v", len(lines), lines)
	}
}

func TestPush_OnAC(t *testing.T) {
	git, repoDir, bareDir := setupRepoWithRemote(t)

	writeFile(t, repoDir, "photo3.raf", "yet more bytes")
	if _, err := Commit(git, repoDir, "ac push commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pushed, err := Push(git, repoDir, true)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !pushed {
		t.Error("pushed=false when onACPower=true, want true")
	}

	// Bare repo must now contain the new commit.
	out, err := exec.Command(git, "-C", bareDir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log bare: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "ac push commit" {
		t.Errorf("bare repo HEAD subject = %q, want %q", got, "ac push commit")
	}
}

// TestPush_FirstPushSetsUpstream mirrors what `framelogd install --remote`
// leaves behind: `git remote add origin <url>` with no push ever having run,
// so the branch has no upstream configured yet. Push must pass -u on this
// first push instead of a plain `git push`, which would fail with "no
// upstream branch".
func TestPush_FirstPushSetsUpstream(t *testing.T) {
	repoDir := t.TempDir()
	bareDir := t.TempDir()

	git := initRepo(t, repoDir)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(bareDir, "init", "--bare")

	writeFile(t, repoDir, "photo.raf", "fake raw bytes")
	if _, err := Commit(git, repoDir, "first commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Wire the remote exactly like `install --remote` does: no -u, no push yet.
	run(repoDir, "remote", "add", "origin", bareDir)

	if hasUpstream(git, repoDir) {
		t.Fatal("hasUpstream=true before any push, want false")
	}

	pushed, err := Push(git, repoDir, true)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !pushed {
		t.Error("pushed=false on first push with remote configured, want true")
	}
	if !hasUpstream(git, repoDir) {
		t.Error("hasUpstream=false after first Push, want true (Push should set tracking)")
	}

	out, err := exec.Command(git, "-C", bareDir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log bare: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "first commit" {
		t.Errorf("bare repo HEAD subject = %q, want %q", got, "first commit")
	}

	// A second push, now that upstream is set, must still work as a plain push.
	writeFile(t, repoDir, "photo2.raf", "more bytes")
	if _, err := Commit(git, repoDir, "second commit"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	pushed, err = Push(git, repoDir, true)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if !pushed {
		t.Error("second pushed=false, want true")
	}
}

// writeFakeBin writes a shell script to dir/<name> that executes body and
// makes it executable. Returns its path.
func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

func TestFindGit_LookPathFallback(t *testing.T) {
	dir := t.TempDir()
	writeFakeBin(t, dir, "git", `echo "fake git"`)

	orig := gitCandidates
	gitCandidates = []string{"/nonexistent/a", "/nonexistent/b"}
	t.Cleanup(func() { gitCandidates = orig })

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	got, err := FindGit()
	if err != nil {
		t.Fatalf("FindGit: %v", err)
	}
	if got == "" {
		t.Error("FindGit returned empty path")
	}
}

func TestFindGit_NoneFound(t *testing.T) {
	orig := gitCandidates
	gitCandidates = []string{"/nonexistent/a", "/nonexistent/b"}
	t.Cleanup(func() { gitCandidates = orig })

	t.Setenv("PATH", t.TempDir()) // empty dir

	_, err := FindGit()
	if err == nil {
		t.Fatal("expected error when git not found, got nil")
	}
	if !strings.Contains(err.Error(), "xcode-select") {
		t.Errorf("error %q missing install hint", err.Error())
	}
}

func TestIsOnACPower_AC(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeBin(t, dir, "pmset", `echo "Now drawing from 'AC Power'"`)

	on, err := IsOnACPower(fake)
	if err != nil {
		t.Fatalf("IsOnACPower: %v", err)
	}
	if !on {
		t.Error("IsOnACPower=false for AC Power output, want true")
	}
}

func TestIsOnACPower_Battery(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeBin(t, dir, "pmset", `echo "Now drawing from 'Battery Power'"`)

	on, err := IsOnACPower(fake)
	if err != nil {
		t.Fatalf("IsOnACPower: %v", err)
	}
	if on {
		t.Error("IsOnACPower=true for Battery Power output, want false")
	}
}

func TestIsOnACPower_EmptyPathSkipsGate(t *testing.T) {
	// pmset absent (path resolved to "") — PROTOCOL.md §6: gate is skipped,
	// which means "treat as on AC" so pushes always proceed.
	on, err := IsOnACPower("")
	if err != nil {
		t.Fatalf("IsOnACPower(\"\"): %v", err)
	}
	if !on {
		t.Error("IsOnACPower(\"\")=false, want true (gate skipped when pmset absent)")
	}
}

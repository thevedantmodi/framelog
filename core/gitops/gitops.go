// Package gitops commits and pushes the originals/ library to its git remote.
// Implementation shells out to the git CLI (not go-git) — this matches the
// Python predecessor's tested behavior exactly, and the interface is small
// enough that swapping to go-git later costs nothing on the caller side.
// Decision recorded in PROTOCOL.md section 6.
//
// The binary-resolution / action split mirrors core/exif exactly: Find* returns
// a path, the action functions take that path as a parameter. This keeps every
// function testable without a real binary on PATH, which matters for pmset
// (macOS-only) in particular — the Python predecessor's git_push called pmset
// directly, silently requiring a real Mac with pmset present in every test run.
package gitops

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gitCandidates is the ordered list of known git binary locations on macOS.
// /usr/bin/git is the Xcode Command Line Tools shim and is present on almost
// every Mac; Homebrew paths are checked as fallbacks.
// Package-level var (not const) so the FindGit test can force the LookPath branch.
var gitCandidates = []string{
	"/usr/bin/git",
	"/opt/homebrew/bin/git",
	"/usr/local/bin/git",
}

// pmsetCandidates is the ordered list of known pmset locations. pmset ships
// with macOS at the system path; the others are included for completeness.
// Package-level var so FindPmset tests can force the LookPath branch.
var pmsetCandidates = []string{
	"/usr/bin/pmset",
	"/opt/homebrew/bin/pmset",
	"/usr/local/bin/pmset",
}

// FindGit returns the absolute path to the git binary. Checks known macOS
// locations first, then falls back to exec.LookPath. Returns an actionable
// error if nothing is found.
func FindGit() (string, error) {
	for _, p := range gitCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("git"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("git not found. Install Xcode Command Line Tools: xcode-select --install")
}

// FindPmset returns the absolute path to the pmset binary. Checks known macOS
// locations first, then falls back to exec.LookPath. Returns an actionable
// error if nothing is found.
func FindPmset() (string, error) {
	for _, p := range pmsetCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("pmset"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("pmset not found (expected on macOS at /usr/bin/pmset)")
}

// Commit stages all changes in originalsPath and commits them with message.
// Returns committed=false (not an error) when there is nothing to commit —
// that is a normal outcome after an ingest that produced no new files.
// The empty-status check mirrors the Python predecessor's git_commit behavior.
func Commit(gitPath, originalsPath, message string) (committed bool, err error) {
	run := func(args ...string) (string, error) {
		var stderr bytes.Buffer
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = originalsPath
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w; stderr: %s", strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
		}
		return string(out), nil
	}

	if _, err := run("add", "-A"); err != nil {
		return false, err
	}

	status, err := run("status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil // nothing staged — not an error
	}

	if _, err := run("commit", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// IsOnACPower runs pmsetPath with "-g batt" and reports whether the output
// contains "AC Power". Takes the resolved binary path as a parameter so callers
// can inject a fake script in tests — the real pmset is macOS-only and must
// never be a hidden test dependency.
func IsOnACPower(pmsetPath string) (bool, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(pmsetPath, "-g", "batt")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("pmset -g batt: %w; stderr: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.Contains(string(out), "AC Power"), nil
}

// HasRemote reports whether originalsPath has at least one git remote configured.
// Push calls this before attempting to push — a repo with no remote is a normal
// first-run state, not an error.
func HasRemote(gitPath, originalsPath string) (bool, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(gitPath, "remote")
	cmd.Dir = originalsPath
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git remote: %w; stderr: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// Push pushes originalsPath to its configured remote when onACPower is true.
// The AC-power check is the caller's responsibility — Push just acts on the
// result. This keeps Push testable with a simple bool argument and no injection
// ceremony: pass true to exercise the push path, false to exercise the skip path.
// Returns pushed=false (not an error) when onACPower is false or no remote is
// configured — a fresh git init with no remote is a normal first-run state.
func Push(gitPath, originalsPath string, onACPower bool) (pushed bool, err error) {
	if !onACPower {
		return false, nil
	}

	ok, err := HasRemote(gitPath, originalsPath)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil // no remote configured — skip silently
	}

	var stderr bytes.Buffer
	cmd := exec.Command(gitPath, "push")
	cmd.Dir = originalsPath
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git push: %w; stderr: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return true, nil
}

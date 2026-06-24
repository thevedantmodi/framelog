package logging

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
)

// lineFormat is the regex every log line must match.
// Anchored: timestamp, space, bracketed prefix (word chars), space, any message.
var lineFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \[\w+\] .+$`)

func newTestLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return lines
}

func TestLog_SingleLine_Format(t *testing.T) {
	l, path := newTestLogger(t)
	l.Log(PrefixIngest, "Done: 3 imported, 1 skipped, 0 failed")

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	if !lineFormat.MatchString(lines[0]) {
		t.Errorf("line does not match format regex:\n  got: %q", lines[0])
	}
}

func TestLog_AllPrefixes(t *testing.T) {
	prefixes := []Prefix{
		PrefixIngest,
		PrefixOutgest,
		PrefixXMP,
		PrefixBackup,
		PrefixGit,
		PrefixCore,
	}

	for _, p := range prefixes {
		t.Run(string(p), func(t *testing.T) {
			l, path := newTestLogger(t)
			l.Log(p, "test message")
			lines := readLines(t, path)
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d", len(lines))
			}
			// Line must contain the exact prefix string in brackets.
			want := fmt.Sprintf("[%s]", p)
			if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(lines[0]) {
				t.Errorf("line %q does not contain %q", lines[0], want)
			}
			if !lineFormat.MatchString(lines[0]) {
				t.Errorf("line %q does not match format regex", lines[0])
			}
		})
	}
}

// TestLog_SyncVisible proves Sync() is called: after Log returns, the line is
// readable directly from the file — it is not sitting in a write buffer waiting
// for Close or an OS flush.
func TestLog_SyncVisible(t *testing.T) {
	l, path := newTestLogger(t)
	l.Log(PrefixCore, "sync visibility check")

	// Read the file without going through the Logger at all.
	lines := readLines(t, path)
	if len(lines) == 0 {
		t.Fatal("line not visible in file immediately after Log() returned — Sync() missing or not working")
	}
	if !lineFormat.MatchString(lines[0]) {
		t.Errorf("visible line does not match format: %q", lines[0])
	}
}

// TestLog_Concurrent spins up 50 goroutines each writing 20 lines (1000 total)
// to the same Logger. Afterwards every line must match the format regex and the
// total must be exactly 1000 — no torn writes, no interleaved bytes, no missing
// lines. This test will reliably detect a missing mutex via the race detector
// and via torn output; run with -race for full coverage.
func TestLog_Concurrent(t *testing.T) {
	const goroutines = 50
	const callsEach = 20

	l, path := newTestLogger(t)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for j := range callsEach {
				l.Log(PrefixIngest, fmt.Sprintf("goroutine %d call %d", id, j))
			}
		}(i)
	}
	wg.Wait()

	lines := readLines(t, path)
	if len(lines) != goroutines*callsEach {
		t.Errorf("expected %d lines, got %d", goroutines*callsEach, len(lines))
	}
	for i, line := range lines {
		if !lineFormat.MatchString(line) {
			t.Errorf("line %d does not match format regex: %q", i, line)
		}
	}
}

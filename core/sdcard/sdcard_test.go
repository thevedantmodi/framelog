package sdcard

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
)

// ---- helpers ----------------------------------------------------------------

func writeFakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writeFakeBin %s: %v", name, err)
	}
	return p
}

func openTestLogger(t *testing.T) *logging.Logger {
	t.Helper()
	l, err := logging.New(filepath.Join(t.TempDir(), "test.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// fakeRunner records how many times RunIngest was called. notifyCh (if set)
// receives a signal each time, so tests can wait without a fixed sleep.
type fakeRunner struct {
	mu       sync.Mutex
	calls    int
	notifyCh chan struct{}
}

func (r *fakeRunner) RunIngest() (ingest.Counts, error) {
	r.mu.Lock()
	r.calls++
	ch := r.notifyCh
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return ingest.Counts{}, nil
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ---- FindDiskutil -----------------------------------------------------------

func TestFindDiskutil_LookPathFallback(t *testing.T) {
	orig := diskutilCandidates
	diskutilCandidates = []string{"/nonexistent/diskutil-a", "/nonexistent/diskutil-b"}
	t.Cleanup(func() { diskutilCandidates = orig })

	binDir := t.TempDir()
	fake := writeFakeBin(t, binDir, "diskutil", `echo "Removable Media: Removable"`)
	_ = fake

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	p, err := FindDiskutil()
	if err != nil {
		t.Fatalf("FindDiskutil: %v", err)
	}
	if p == "" {
		t.Fatal("FindDiskutil returned empty path")
	}
}

func TestFindDiskutil_NotFound(t *testing.T) {
	orig := diskutilCandidates
	diskutilCandidates = []string{"/nonexistent/diskutil-a"}
	t.Cleanup(func() { diskutilCandidates = orig })

	t.Setenv("PATH", t.TempDir()) // empty bin dir — nothing on PATH

	_, err := FindDiskutil()
	if err == nil {
		t.Fatal("FindDiskutil returned nil error when binary absent")
	}
}

// ---- IsRemovableMedia -------------------------------------------------------

func TestIsRemovableMedia_True(t *testing.T) {
	binDir := t.TempDir()
	fake := writeFakeBin(t, binDir, "diskutil",
		`echo "   Removable Media:           Removable"`)

	got, err := IsRemovableMedia(fake, t.TempDir())
	if err != nil {
		t.Fatalf("IsRemovableMedia: %v", err)
	}
	if !got {
		t.Error("IsRemovableMedia = false, want true for Removable output")
	}
}

func TestIsRemovableMedia_False(t *testing.T) {
	binDir := t.TempDir()
	fake := writeFakeBin(t, binDir, "diskutil",
		`echo "   Removable Media:           Fixed"`)

	got, err := IsRemovableMedia(fake, t.TempDir())
	if err != nil {
		t.Fatalf("IsRemovableMedia: %v", err)
	}
	if got {
		t.Error("IsRemovableMedia = true, want false for Fixed output")
	}
}

// ---- HasDCIM ----------------------------------------------------------------

func TestHasDCIM_True(t *testing.T) {
	vol := t.TempDir()
	if err := os.Mkdir(filepath.Join(vol, "DCIM"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !HasDCIM(vol) {
		t.Error("HasDCIM = false, want true when DCIM subdir exists")
	}
}

func TestHasDCIM_False(t *testing.T) {
	vol := t.TempDir()
	if HasDCIM(vol) {
		t.Error("HasDCIM = true, want false when no DCIM subdir")
	}
}

// ---- FindSDCard -------------------------------------------------------------

// TestFindSDCard builds a fake Volumes-shaped directory with three entries:
//   - SDCard  — DCIM present, diskutil reports Removable → should be returned
//   - FixedDCIM — DCIM present, diskutil reports Fixed → skip
//   - NoDCIM — no DCIM, diskutil reports Removable → skip
func TestFindSDCard_ReturnsCorrectVolume(t *testing.T) {
	volumes := t.TempDir()
	binDir := t.TempDir()

	sdCard := filepath.Join(volumes, "SDCard")
	fixed := filepath.Join(volumes, "FixedDCIM")
	noDCIM := filepath.Join(volumes, "NoDCIM")
	for _, d := range []string{sdCard, fixed, noDCIM} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// SDCard and FixedDCIM have DCIM; NoDCIM does not.
	for _, d := range []string{sdCard, fixed} {
		if err := os.Mkdir(filepath.Join(d, "DCIM"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Fake diskutil: Removable for paths ending in SDCard or NoDCIM, Fixed otherwise.
	fake := writeFakeBin(t, binDir, "diskutil", `
case "$2" in
  */SDCard|*/NoDCIM) echo "   Removable Media:           Removable" ;;
  *)                 echo "   Removable Media:           Fixed" ;;
esac`)

	got, err := FindSDCard(fake, volumes)
	if err != nil {
		t.Fatalf("FindSDCard: %v", err)
	}
	if got != sdCard {
		t.Errorf("FindSDCard = %q, want %q", got, sdCard)
	}
}

func TestFindSDCard_NoneMatch_ReturnsEmpty(t *testing.T) {
	volumes := t.TempDir()
	binDir := t.TempDir()

	// One entry, no DCIM, diskutil says Fixed.
	if err := os.Mkdir(filepath.Join(volumes, "BackupDrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeBin(t, binDir, "diskutil",
		`echo "   Removable Media:           Fixed"`)

	got, err := FindSDCard(fake, volumes)
	if err != nil {
		t.Fatalf("FindSDCard: %v", err)
	}
	if got != "" {
		t.Errorf("FindSDCard = %q, want empty string when nothing matches", got)
	}
}

// ---- CopyDCIM ---------------------------------------------------------------

func TestCopyDCIM_CopiesStructureAndCount(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// DCIM/100CANON/IMG_0001.JPG and IMG_0002.JPG
	canon := filepath.Join(src, "100CANON")
	if err := os.Mkdir(canon, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "IMG_0001.JPG"), []byte("photo1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "IMG_0002.JPG"), []byte("photo2"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := CopyDCIM(src, dst)
	if err != nil {
		t.Fatalf("CopyDCIM: %v", err)
	}
	if n != 2 {
		t.Errorf("copied count = %d, want 2", n)
	}

	// Structure preserved: dst/100CANON/IMG_0001.JPG
	for _, name := range []string{"IMG_0001.JPG", "IMG_0002.JPG"} {
		p := filepath.Join(dst, "100CANON", name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s in dest: %v", name, err)
		}
	}
}

func TestCopyDCIM_NoClobber(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	canon := filepath.Join(src, "100CANON")
	if err := os.Mkdir(canon, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "IMG_0001.JPG"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "IMG_0002.JPG"), []byte("photo2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First copy — both files land.
	if _, err := CopyDCIM(src, dst); err != nil {
		t.Fatalf("first CopyDCIM: %v", err)
	}

	// Independently modify the dest copy of IMG_0001.JPG to a sentinel value.
	destFile := filepath.Join(dst, "100CANON", "IMG_0001.JPG")
	if err := os.WriteFile(destFile, []byte("PROTECTED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Add a third file to src.
	if err := os.WriteFile(filepath.Join(canon, "IMG_0003.JPG"), []byte("photo3"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second copy — only the new file should be copied.
	n, err := CopyDCIM(src, dst)
	if err != nil {
		t.Fatalf("second CopyDCIM: %v", err)
	}
	if n != 1 {
		t.Errorf("second copy count = %d, want 1 (only the new file)", n)
	}

	// Sentinel content must be intact — no-clobber protected it.
	content, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("read dest file: %v", err)
	}
	if string(content) != "PROTECTED" {
		t.Errorf("dest file content = %q, want %q (no-clobber violated)", content, "PROTECTED")
	}
}

// ---- Watcher integration ----------------------------------------------------

// TestWatcher_Integration verifies end-to-end: creating an SD-card-shaped
// directory under a temp Volumes root triggers exactly one RunIngest call, and
// the DCIM file lands in InboxPath. Creating a second directory without DCIM
// must NOT trigger a second call.
//
// The 2 s settle delay in Run() means this test takes ~3 s per event. It is
// skipped in -short mode.
func TestWatcher_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping watcher integration test: requires 2s settle delays")
	}

	volumes := t.TempDir()
	inbox := t.TempDir()
	binDir := t.TempDir()

	// Fake diskutil: always reports Removable, regardless of volume path.
	// HasDCIM controls the final gate for the second (no-DCIM) directory.
	diskutil := writeFakeBin(t, binDir, "diskutil",
		`echo "   Removable Media:           Removable"`)

	notifyCh := make(chan struct{}, 1)
	runner := &fakeRunner{notifyCh: notifyCh}
	logger := openTestLogger(t)

	w := &Watcher{
		DiskutilPath: diskutil,
		VolumesRoot:  volumes,
		InboxPath:    inbox,
		Runner:       runner,
		Logger:       logger,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- w.Run() }()

	// Give fsnotify time to establish the watch before creating directories.
	time.Sleep(200 * time.Millisecond)

	// --- Create an SD card: subdir + DCIM + one file inside. ---
	sdPath := filepath.Join(volumes, "SDCARD")
	if err := os.MkdirAll(filepath.Join(sdPath, "DCIM", "100CANON"), 0o755); err != nil {
		t.Fatalf("create DCIM tree: %v", err)
	}
	photoSrc := filepath.Join(sdPath, "DCIM", "100CANON", "IMG_0001.JPG")
	if err := os.WriteFile(photoSrc, []byte("photo bytes"), 0o644); err != nil {
		t.Fatalf("write photo: %v", err)
	}

	// Wait for 2s settle + processing with a hard deadline.
	var called int32
	select {
	case <-notifyCh:
		atomic.StoreInt32(&called, 1)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: RunIngest was not called within 5s of SD card creation")
	}

	if runner.callCount() != 1 {
		t.Errorf("runner called %d times after SD card mount, want 1", runner.callCount())
	}

	// File must be in InboxPath preserving the DCIM subdirectory structure.
	inboxFile := filepath.Join(inbox, "100CANON", "IMG_0001.JPG")
	if _, err := os.Stat(inboxFile); err != nil {
		t.Errorf("photo not found in inbox at %s: %v", inboxFile, err)
	}

	// --- Create a directory with no DCIM — must NOT trigger a second call. ---
	otherPath := filepath.Join(volumes, "BackupDrive")
	if err := os.Mkdir(otherPath, 0o755); err != nil {
		t.Fatalf("create other dir: %v", err)
	}

	// Wait for the 2s settle to elapse (watcher will check and skip on no DCIM).
	time.Sleep(3 * time.Second)

	if runner.callCount() != 1 {
		t.Errorf("runner called %d times after non-SD-card dir, want still 1",
			runner.callCount())
	}

	w.Stop()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() returned non-nil error after Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run() did not return after Stop()")
	}

	_ = called
	_ = fmt.Sprintf // keep fmt import used via Sprintf in fakeRunner
}

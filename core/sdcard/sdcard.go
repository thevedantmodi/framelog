// Package sdcard detects SD cards mounted under /Volumes, copies their camera
// media into inbox/ via rclone, and fires RunIngest. It ports the detection
// logic from on_sd_mount.sh — the dual check (diskutil says removable AND
// camera media is present) naturally excludes other things that mount under
// /Volumes (backup drives, network shares) without any explicit
// special-casing. "Camera media" is DCIM or Sony's PRIVATE/M4ROOT/CLIP: XAVC
// bodies keep video out of DCIM entirely, so a video-only card has no DCIM
// and would otherwise never be picked up.
//
// The injectable-binary-path pattern from core/exif and core/gitops is
// applied here for both diskutil and rclone: FindDiskutil/backup.FindRclone
// return paths, IsRemovableMedia/CopyDCIM take them as parameters. Tests can
// therefore pass a fake shell script with no real diskutil or rclone present
// — which matters because diskutil only exists on macOS and rclone may not
// be installed. rclone is a hard dependency for the SD card watcher (like
// diskutil): main.go only constructs a Watcher when both are found. CopyDCIM
// delegates the actual rclone invocation and JSON-log progress parsing to
// core/rclonerun, shared with core/backup's Sync.
package sdcard

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/cardverify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/nrtmeta"
	"github.com/thevedantmodi/framelog/core/rclonerun"
)

// ErrNoCard is returned by VerifyMounted when no camera card is mounted.
// Wire-string convention (PROTOCOL.md §3 verify_card).
var ErrNoCard = errors.New("no_card")

// ErrCardBusy is returned by VerifyMounted while a card is already being
// copied or verified.
var ErrCardBusy = errors.New("card_busy")

// ErrVerifyUnavailable is returned by VerifyMounted when the watcher was
// built without a catalog DB (verification disabled).
var ErrVerifyUnavailable = errors.New("verify_unavailable")

// diskutilCandidates is the ordered list of known diskutil locations on macOS.
// Package-level var (not const) so FindDiskutil tests can force the LookPath branch.
var diskutilCandidates = []string{
	"/usr/sbin/diskutil",
	"/usr/bin/diskutil",
}

// FindDiskutil returns the absolute path to the diskutil binary. Checks known
// macOS locations first, then falls back to exec.LookPath. Returns an
// actionable error if neither finds it.
func FindDiskutil() (string, error) {
	for _, p := range diskutilCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("diskutil"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("diskutil not found (expected on macOS at /usr/sbin/diskutil)")
}

// IsRemovableMedia runs `<diskutilPath> info <volPath>` and reports whether
// any output line matches "Removable Media:.*Removable" — the same grep
// pattern the original on_sd_mount.sh used. Taking the resolved binary as a
// parameter (not calling FindDiskutil internally) makes this testable with a
// fake script, mirroring ReadExif and IsOnACPower.
func IsRemovableMedia(diskutilPath, volPath string) (bool, error) {
	out, err := exec.Command(diskutilPath, "info", volPath).Output()
	if err != nil {
		return false, fmt.Errorf("diskutil info %s: %w", volPath, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, "Removable Media:")
		if idx < 0 {
			continue
		}
		// Check only the value part (after "Removable Media:") so that the word
		// "Removable" in the key name does not cause a false positive when the
		// value is "Fixed" or "Not Removable".
		value := line[idx+len("Removable Media:"):]
		if strings.Contains(value, "Removable") && !strings.Contains(value, "Not Removable") {
			return true, nil
		}
	}
	return false, nil
}

// VolumeUUID returns the "Volume UUID" diskutil reports for volPath, or ""
// when diskutil fails or reports none. Card volume names are useless as an
// identity — every Sony card formats as "Untitled", most others "NO NAME" —
// so reports carry the UUID (which changes on every in-camera format) to
// tell one card, and one format of it, from another.
func VolumeUUID(diskutilPath, volPath string) string {
	out, err := exec.Command(diskutilPath, "info", volPath).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, v, ok := strings.Cut(line, "Volume UUID:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// HasDCIM reports whether volPath contains a DCIM subdirectory. Not injected
// because it is just a directory check — no external binary involved.
func HasDCIM(volPath string) bool {
	return isDir(filepath.Join(volPath, config.DCIMDirName))
}

// HasClips reports whether volPath contains a Sony XAVC clip directory
// (PRIVATE/M4ROOT/CLIP). Sony bodies write video there, not under DCIM, so a
// card that has only been used for video has no DCIM at all — checking for
// DCIM alone would leave it undetected.
func HasClips(volPath string) bool {
	return isDir(filepath.Join(volPath, config.ClipDirRelPath))
}

// IsCameraCard reports whether volPath looks like a camera card: it holds
// stills (DCIM), clips (PRIVATE/M4ROOT/CLIP), or both. Paired with the
// diskutil removable check, this is the dual test that keeps backup drives
// and network shares out without special-casing them.
func IsCameraCard(volPath string) bool {
	return HasDCIM(volPath) || HasClips(volPath)
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// FindSDCard lists the immediate subdirectories of volumesRoot, and returns
// the first one (in name order) that is both removable media and contains a
// camera media (DCIM and/or PRIVATE/M4ROOT/CLIP). Returns "", nil when no
// match is found — "no SD card present" is a normal outcome, not a failure.
func FindSDCard(diskutilPath, volumesRoot string) (string, error) {
	entries, err := os.ReadDir(volumesRoot)
	if err != nil {
		return "", fmt.Errorf("sdcard: readdir %s: %w", volumesRoot, err)
	}
	// os.ReadDir already returns entries sorted by name, matching the bash
	// loop's implicit alphabetical order over glob results.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volPath := filepath.Join(volumesRoot, e.Name())
		removable, err := IsRemovableMedia(diskutilPath, volPath)
		if err != nil {
			continue // diskutil error for one volume shouldn't stop the scan
		}
		if removable && IsCameraCard(volPath) {
			return volPath, nil
		}
	}
	return "", nil
}

// CopyDCIM recursively copies the contents of sdDCIMPath into inboxPath via
// `rclone copy`, preserving relative directory structure
// (DCIM/100CANON/IMG_0001.JPG → inbox/100CANON/IMG_0001.JPG).
// --ignore-existing matches the old `cp -rn` semantics: non-clobbering — if a
// file already exists at the destination it is skipped entirely, protecting a
// file left over from a previous interrupted run. --include restricts the
// copy to the current import types (config.ImportExtensions); --ignore-case makes that match
// regardless of the card's filename casing (IMG_0001.JPG vs .jpg).
// Returns the count of files actually copied (not counting skips).
//
// The optional onCopy callback is invoked after each successful file copy with
// the source file's base name and the running copied-so-far count, parsed
// from rclone's JSON log stream by rclonerun.Copy. Pass nil (or omit) when
// progress reporting is not needed.
func CopyDCIM(rclonePath, sdDCIMPath, inboxPath string, onCopy ...func(filename string, n int)) (int, error) {
	return copyMedia(rclonePath, sdDCIMPath, inboxPath, sortedSupportedExtensions(), onCopy...)
}

// CopyClips copies a Sony XAVC clip directory (PRIVATE/M4ROOT/CLIP) into
// inboxPath with the same non-clobbering semantics as CopyDCIM. The include
// list is the import types plus config.NRTMetaExtension: the C####M01.XML
// sidecar must land in inbox next to its clip, because it — not the MP4
// container — carries the local capture time, body, and lens that ingest
// files the clip by. Nothing else on the card's PRIVATE tree is copied.
func CopyClips(rclonePath, sdClipPath, inboxPath string, onCopy ...func(filename string, n int)) (int, error) {
	exts := append(sortedSupportedExtensions(), config.NRTMetaExtension)
	return copyMedia(rclonePath, sdClipPath, inboxPath, exts, onCopy...)
}

// copyMedia is the shared rclone invocation behind CopyDCIM and CopyClips.
func copyMedia(rclonePath, srcPath, inboxPath string, exts []string, onCopy ...func(filename string, n int)) (int, error) {
	var cb func(string, int)
	if len(onCopy) > 0 {
		cb = onCopy[0]
	}

	res, err := copyMediaDetailed(rclonePath, srcPath, inboxPath, exts, cb)
	return res.Copied, err
}

func copyMediaDetailed(rclonePath, srcPath, inboxPath string, exts []string, cb func(string, int)) (rclonerun.Result, error) {
	args := []string{"copy", srcPath, inboxPath, "--ignore-existing", "--ignore-case"}
	for _, ext := range exts {
		args = append(args, "--include", "*"+ext)
	}
	return rclonerun.Run(rclonePath, args, cb)
}

// sortedSupportedExtensions returns the current import types, sorted, so the
// rclone --include argument list is deterministic. The set is user-settable
// at runtime (config.SetImportExtensions), so it is read on every copy.
func sortedSupportedExtensions() []string {
	return config.ImportExtensions()
}

// Watcher polls VolumesRoot for new mounts, detects SD cards, and fires
// RunIngest after copying DCIM contents to InboxPath.
//
// macOS volume mounts are kernel-level synthetic filesystem ops not delivered
// by FSEvents or kqueue; polling /Volumes every PollInterval is the reliable
// fix. Cost is negligible — os.ReadDir on a directory with ~5 entries.
type Watcher struct {
	DiskutilPath string
	RclonePath   string
	VolumesRoot  string
	InboxPath    string
	PollInterval time.Duration
	Runner       ingest.Runner
	Logger       *logging.Logger

	// --- End-to-end verification (cardverify). Leave DB nil to disable. ---

	// DB is the catalog verification checks card hashes against.
	DB *sql.DB
	// OriginalsPath is the library root (backup copies mirror its layout).
	OriginalsPath string
	// ReportsDir receives one plain-text receipt per verified card.
	ReportsDir string
	// BackupPath returns the currently configured backup root ("" = none).
	// A func, not a string, because set_backup_path changes it at runtime.
	BackupPath func() string
	// Activity receives live copy/verify progress. Nil-safe.
	Activity *activity.Tracker
	// OnReport, when set, is called with every saved card report.
	OnReport func(db.CardReport)
	// OnPreview, when set, is called with each new card's preview.
	OnPreview func(*cardverify.Preview)
	// IngestRetryInterval is how often a card waiting on an
	// already-running ingest re-tries (default 2s); IngestRetryTimeout
	// bounds the wait (default 30m).
	IngestRetryInterval time.Duration
	IngestRetryTimeout  time.Duration

	// cardMu serialises card work: the automatic copy→ingest→verify run
	// and on-demand VerifyMounted never overlap.
	cardMu sync.Mutex

	// preview is the quick "what's new" result for the mounted card; it is
	// cleared when that card is unmounted.
	previewMu sync.Mutex
	preview   *cardverify.Preview

	// seen is the set of volume names present on the last tick.
	// processed is the set already handled this mount session.
	// A name leaving seen is evicted from processed so re-insertion fires again.
	seen      map[string]bool
	processed map[string]bool
	mu        sync.Mutex
}

// Run polls VolumesRoot every PollInterval (default 2s) until stop is closed.
func (w *Watcher) Run(stop <-chan struct{}) error {
	interval := w.PollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}

	w.mu.Lock()
	w.seen = make(map[string]bool)
	w.processed = make(map[string]bool)
	w.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *Watcher) tick() {
	entries, err := os.ReadDir(w.VolumesRoot)
	if err != nil {
		w.Logger.Log(logging.PrefixCore,
			fmt.Sprintf("sdcard: readdir %s: %v", w.VolumesRoot, err))
		return
	}

	current := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			current[e.Name()] = true
		}
	}

	w.mu.Lock()
	seen := w.seen
	processed := w.processed
	w.mu.Unlock()

	// Evict unmounted volumes so re-insertion fires again.
	for name := range seen {
		if !current[name] {
			delete(processed, name)
			w.clearPreview(name)
		}
	}

	// pausedNames collects SD cards seen this tick that were skipped because the
	// pipeline is paused. They're excluded from w.seen below so they look "new"
	// again on the next tick and get retried once resumed, rather than
	// requiring a physical unmount/remount.
	var pausedNames []string

	// Check newly-appeared volumes.
	for name := range current {
		if seen[name] || processed[name] {
			continue
		}
		seen[name] = true // mark seen regardless of whether it's an SD card

		volPath := filepath.Join(w.VolumesRoot, name)
		removable, err := IsRemovableMedia(w.DiskutilPath, volPath)
		if err != nil {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("diskutil error for %s: %v", name, err))
			continue
		}
		if !removable || !IsCameraCard(volPath) {
			continue
		}
		if w.Runner.Paused() {
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("ingest paused, SD card %s left for retry on resume", volPath))
			pausedNames = append(pausedNames, name)
			continue
		}

		processed[name] = true
		w.processCard(name, volPath)
	}

	for _, name := range pausedNames {
		delete(current, name)
	}

	w.mu.Lock()
	w.seen = current
	w.mu.Unlock()
}

// processCard is the full automatic run for one newly inserted card:
// preview → copy what's new → ingest (which backs up) → end-to-end
// verification → report. Without a DB (verification disabled) it falls back
// to copying every importable file and stops after ingest.
func (w *Watcher) processCard(name, volPath string) {
	w.cardMu.Lock()
	defer w.cardMu.Unlock()
	defer w.Activity.Idle()

	sessionStart := time.Now()
	uuid := VolumeUUID(w.DiskutilPath, volPath)
	w.Logger.Log(logging.PrefixCore, "SD card detected: "+volPath)

	// 1. Preview: which files are new? Seconds, not minutes (quick
	// fingerprints). If it fails, copy everything — the old behaviour.
	var preview *cardverify.Preview
	if w.DB != nil {
		w.Activity.Start(activity.PhaseScanning, name, 0)
		p, err := cardverify.PreviewCard(volPath, name, w.DB)
		if err != nil {
			w.Logger.Log(logging.PrefixCore, fmt.Sprintf("card preview failed, copying everything: %v", err))
		} else {
			preview = p
			w.setPreview(p)
			w.Logger.Log(logging.PrefixCore, fmt.Sprintf("card %s: %s", name, p.Summary()))
			if w.OnPreview != nil {
				w.OnPreview(p)
			}
		}
	}

	// 2. Copy.
	var copyProblems []string
	if preview != nil {
		copyProblems = w.copyListed(name, volPath, preview.New)
	} else {
		w.Logger.Log(logging.PrefixCore, "scanning card (may take a moment on slow card readers)...")
		copyProblems = w.copyAll(name, volPath)
	}

	// 3. Import (and back up).
	w.runIngestWaiting()

	if w.DB == nil {
		return
	}

	// 4. Verify. A quick fingerprint is a hint: if it wrongly matched a
	// file that is not really in the library, that file was skipped by the
	// copy and verification reports it as not imported. Copy those once
	// and check again, so a wrong hint costs time, never a lost photo.
	var copiedSet map[string]bool
	if preview != nil {
		copiedSet = make(map[string]bool, len(preview.New))
		for _, f := range preview.New {
			copiedSet[f.RelPath] = true
		}
	}
	rep, err := w.check(name, volPath, uuid, sessionStart, copyProblems, copiedSet)
	if err != nil {
		w.Logger.Log(logging.PrefixCore, fmt.Sprintf("card verification error: %v", err))
		return
	}
	if preview != nil {
		if missed := skippedButMissing(rep, preview); len(missed) > 0 {
			w.Logger.Log(logging.PrefixCore, fmt.Sprintf(
				"quick check missed %d file(s) not in the library — copying them now", len(missed)))
			copyProblems = append(copyProblems, w.copyListed(name, volPath, missed)...)
			for _, f := range missed {
				copiedSet[f.RelPath] = true
			}
			w.runIngestWaiting()
			if rep, err = w.check(name, volPath, uuid, sessionStart, copyProblems, copiedSet); err != nil {
				w.Logger.Log(logging.PrefixCore, fmt.Sprintf("card verification error: %v", err))
				return
			}
		}
	}
	if _, err := w.save(rep); err != nil {
		w.Logger.Log(logging.PrefixCore, fmt.Sprintf("card report error: %v", err))
	}
}

// skippedButMissing returns the media files verification found not in the
// library that the preview had judged already imported (so they were never
// copied). Files the preview called new are not returned: they were copied,
// and if they still aren't imported the cause is the import, not the hint.
func skippedButMissing(rep *cardverify.Report, preview *cardverify.Preview) []cardverify.CardFile {
	copied := make(map[string]bool, len(preview.New))
	for _, f := range preview.New {
		copied[f.RelPath] = true
	}
	var out []cardverify.CardFile
	for _, f := range rep.Files {
		if f.State == cardverify.StateNotInLibrary && !copied[f.RelPath] {
			out = append(out, f.CardFile)
		}
	}
	return out
}

// copyAll copies every importable file from the card into inbox/ (rclone
// --include filters, --ignore-existing). Used when there is no preview.
func (w *Watcher) copyAll(name, volPath string) []string {
	total := 0
	if inv, err := cardverify.Inventory(volPath); err == nil {
		for _, f := range inv {
			if f.Kind == cardverify.KindMedia || f.Kind == cardverify.KindSidecar {
				total++
			}
		}
	}
	w.Activity.Start(activity.PhaseCopying, name, total)

	var problems []string
	n := 0
	part := func(label, src string, exts []string) {
		base := n
		res, err := copyMediaDetailed(w.RclonePath, src, w.InboxPath, exts, w.copyProgress(base))
		problems = append(problems, w.copyResult(label, res, err)...)
		n += res.Copied
	}
	// DCIM and PRIVATE/M4ROOT/CLIP are copied independently: a card can
	// hold either or both, and a failure reading one must not skip the
	// other. Both land flat in inbox/ so a clip's XML sidecar stays next
	// to it, which is what ingest's sidecar lookup relies on.
	if HasDCIM(volPath) {
		part(config.DCIMDirName, filepath.Join(volPath, config.DCIMDirName), sortedSupportedExtensions())
	}
	if HasClips(volPath) {
		part(config.ClipDirRelPath, filepath.Join(volPath, config.ClipDirRelPath),
			append(sortedSupportedExtensions(), config.NRTMetaExtension))
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("copied %d files from card", n))
	return problems
}

// copyListed copies exactly the given card files into inbox/, plus the Sony
// .XML sidecar of every clip among them (ingest needs it next to the clip).
// Files keep their path below DCIM/ or the clip folder, as copyAll does.
func (w *Watcher) copyListed(name, volPath string, files []cardverify.CardFile) []string {
	roots := []struct{ label, rel string }{
		{config.DCIMDirName, config.DCIMDirName},
		{config.ClipDirRelPath, config.ClipDirRelPath},
	}
	lists := make([][]string, len(roots))
	total := 0
	for _, f := range files {
		for i, r := range roots {
			prefix := r.rel + string(filepath.Separator)
			if !strings.HasPrefix(f.RelPath, prefix) {
				continue
			}
			lists[i] = append(lists[i], strings.TrimPrefix(f.RelPath, prefix))
			total++
			if i == 1 {
				if sc := nrtmeta.FindSidecar(f.AbsPath); sc != "" {
					lists[i] = append(lists[i], strings.TrimPrefix(
						strings.TrimPrefix(sc, filepath.Join(volPath, r.rel)), string(filepath.Separator)))
					total++
				}
			}
		}
	}
	if total == 0 {
		w.Logger.Log(logging.PrefixCore, "nothing new to copy from card")
		return nil
	}

	w.Activity.Start(activity.PhaseCopying, name, total)
	var problems []string
	n := 0
	for i, r := range roots {
		if len(lists[i]) == 0 {
			continue
		}
		res, err := copyFileList(w.RclonePath, filepath.Join(volPath, r.rel), w.InboxPath, lists[i], w.copyProgress(n))
		problems = append(problems, w.copyResult(r.label, res, err)...)
		n += res.Copied
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("copied %d new files from card", n))
	return problems
}

func (w *Watcher) copyProgress(base int) func(string, int) {
	return func(filename string, i int) {
		w.Activity.Step(base+i, filename)
		w.Logger.Log(logging.PrefixCore, fmt.Sprintf("copying [%05d] %s → inbox/", base+i, filename))
	}
}

// copyResult logs one rclone run and turns a failure into a report problem.
func (w *Watcher) copyResult(label string, res rclonerun.Result, err error) []string {
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("copied %d files from %s", res.Copied, label))
	if err == nil && len(res.Errors) == 0 {
		return nil
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("%s copy error: %v", label, err))
	return []string{fmt.Sprintf("Copying %s from the card failed: %s", label, rclonerun.Explain(res, err))}
}

// copyFileList runs `rclone copy src inbox --files-from-raw <list>`, copying
// exactly the listed paths (relative to src). rclone skips directory
// listing entirely in this mode, which is also what makes re-inserting a
// mostly-imported card fast.
func copyFileList(rclonePath, src, inbox string, rels []string, cb func(string, int)) (rclonerun.Result, error) {
	f, err := os.CreateTemp("", "framelog-files-*.txt")
	if err != nil {
		return rclonerun.Result{}, err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(strings.Join(rels, "\n") + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return rclonerun.Result{}, werr
	}
	args := []string{"copy", src, inbox, "--files-from-raw", f.Name(), "--ignore-existing"}
	return rclonerun.Run(rclonePath, args, cb)
}

// CurrentPreview returns the preview of the card that is mounted now, or nil.
func (w *Watcher) CurrentPreview() *cardverify.Preview {
	w.previewMu.Lock()
	defer w.previewMu.Unlock()
	return w.preview
}

func (w *Watcher) setPreview(p *cardverify.Preview) {
	w.previewMu.Lock()
	w.preview = p
	w.previewMu.Unlock()
}

// clearPreview forgets the preview once its card is unmounted.
func (w *Watcher) clearPreview(volumeName string) {
	w.previewMu.Lock()
	if w.preview != nil && w.preview.VolumeName == volumeName {
		w.preview = nil
	}
	w.previewMu.Unlock()
}

// runIngestWaiting runs ingest for the freshly copied card. If a manual
// ingest is already running it waits for it rather than skipping: skipping
// would leave this card's files in inbox/ and the verification that follows
// would (correctly, but needlessly) say "do not format".
func (w *Watcher) runIngestWaiting() {
	interval := w.IngestRetryInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timeout := w.IngestRetryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	logged := false
	for {
		counts, err := w.Runner.RunIngest()
		switch {
		case err == nil:
			w.Logger.Log(logging.PrefixCore,
				fmt.Sprintf("ingest done: %d imported, %d skipped, %d failed",
					counts.Imported, counts.Skipped, counts.Failed))
			return
		case errors.Is(err, ingest.ErrIngestAlreadyRunning) && time.Now().Before(deadline):
			if !logged {
				w.Logger.Log(logging.PrefixCore, "ingest already running, waiting to import SD card")
				logged = true
			}
			time.Sleep(interval)
		case errors.Is(err, ingest.ErrIngestAlreadyRunning):
			w.Logger.Log(logging.PrefixCore,
				"ingest already running, skipping SD card trigger")
			return
		default:
			w.Logger.Log(logging.PrefixCore, fmt.Sprintf("ingest error: %v", err))
			return
		}
	}
}

// check runs cardverify against the card and returns the unsaved report.
// Caller holds cardMu.
func (w *Watcher) check(name, volPath, uuid string, sessionStart time.Time, extra []string, copied map[string]bool) (*cardverify.Report, error) {
	backupPath := ""
	if w.BackupPath != nil {
		backupPath = w.BackupPath()
	}
	target := "library"
	if backupPath != "" {
		target = "library and backup"
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf("verifying card %s against %s", name, target))
	w.Activity.Start(activity.PhaseVerifying, name, 0)
	return cardverify.Verify(cardverify.Options{
		VolumePath:        volPath,
		VolumeName:        name,
		VolumeUUID:        uuid,
		DB:                w.DB,
		OriginalsPath:     w.OriginalsPath,
		BackupPath:        backupPath,
		SessionStart:      sessionStart,
		ExtraProblems:     extra,
		CopiedThisSession: copied,
		Progress: func(done, total int, file string) {
			w.Activity.SetTotal(total)
			w.Activity.Step(done, file)
		},
	})
}

// save stores the report and receipt, logs the verdict, and notifies
// OnReport.
func (w *Watcher) save(rep *cardverify.Report) (db.CardReport, error) {
	sum, err := cardverify.Save(w.DB, rep, w.ReportsDir)
	if err != nil {
		return sum, err
	}
	verdict := "DO NOT FORMAT"
	if sum.SafeToFormat {
		verdict = "SAFE TO FORMAT"
	}
	w.Logger.Log(logging.PrefixCore, fmt.Sprintf(
		"card %s: %s — %d/%d files verified (%s), backup %s",
		rep.VolumeName, verdict, sum.FilesVerified, sum.FilesTotal,
		cardverify.HumanBytes(sum.BytesTotal), sum.BackupState))
	for _, p := range sum.Problems {
		w.Logger.Log(logging.PrefixCore, "  problem: "+p)
	}
	if sum.ReportPath != "" {
		w.Logger.Log(logging.PrefixCore, "card report: "+sum.ReportPath)
	}
	if w.OnReport != nil {
		w.OnReport(sum)
	}
	return sum, nil
}

// VerifyMounted re-checks the currently mounted camera card without copying
// or importing anything — the "Verify Card" menu action. Use it after
// plugging the backup drive back in, or right before formatting, to get a
// fresh verdict. Read-only on the card, so it runs even while paused.
func (w *Watcher) VerifyMounted() (db.CardReport, error) {
	if w.DB == nil {
		return db.CardReport{}, ErrVerifyUnavailable
	}
	if !w.cardMu.TryLock() {
		return db.CardReport{}, ErrCardBusy
	}
	defer w.cardMu.Unlock()
	defer w.Activity.Idle()

	volPath, err := FindSDCard(w.DiskutilPath, w.VolumesRoot)
	if err != nil {
		return db.CardReport{}, err
	}
	if volPath == "" {
		return db.CardReport{}, ErrNoCard
	}
	name := filepath.Base(volPath)
	rep, err := w.check(name, volPath, VolumeUUID(w.DiskutilPath, volPath), time.Time{}, nil, nil)
	if err != nil {
		return db.CardReport{}, err
	}
	return w.save(rep)
}

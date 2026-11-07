// Package sim runs the real Framelog pipeline end to end against a fake
// camera card in a throwaway sandbox, so you can see it work without an SD
// card, a card reader, or any photos.
//
// Everything that matters is the production code: the SD card watcher polls
// a Volumes/ folder, previews and copies the card, ingest imports into a
// real originals/ git repo and catalog, backup copies to a "drive" folder,
// verification re-hashes everything, and the IPC socket answers the same
// JSON the menu bar app reads. Only the outside world is faked: the card is
// a folder of generated files laid out like a Sony α7C II card, and
// diskutil, exiftool, rclone and pmset are small shell scripts (see
// tools.go). Pass RealRclone to use the installed rclone instead.
//
// "Inserting" the card renames it into Volumes/; "ejecting" renames it out.
// Each scenario then checks what a photographer would check: the preview,
// the verdict, the backup state, and what the status socket reports.
//
// Run it with `make sim` (narrated) or `go test ./sim` (pass/fail).
package sim

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thevedantmodi/framelog/core/activity"
	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/cardverify"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/ipc"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
	"github.com/thevedantmodi/framelog/core/sdcard"
)

// Options configures a simulation run.
type Options struct {
	// Dir is the sandbox root. Empty means a new temporary directory.
	Dir string
	// Out receives the narration. nil means io.Discard.
	Out io.Writer
	// PipelineLog, when set, also receives every framelog.log line.
	PipelineLog io.Writer
	// RealRclone uses the installed rclone instead of the fake. Scenarios
	// that need to make rclone fail on purpose are then skipped.
	RealRclone bool
	// Only runs just the scenarios whose IDs are listed (all when empty).
	// Scenarios build on each other, so earlier ones always run first.
	Only []string
}

// Result is the outcome of one scenario.
type Result struct {
	ID       string
	Title    string
	Passed   bool
	Skipped  bool
	Failures []string
	Notes    []string
	Duration time.Duration
}

// Summary is everything Run learned.
type Summary struct {
	Dir        string // sandbox root
	ReportsDir string // card receipts
	LogFile    string // framelog.log
	Results    []Result
}

// Passed reports whether every scenario that ran passed.
func (s Summary) Passed() bool {
	for _, r := range s.Results {
		if !r.Passed && !r.Skipped {
			return false
		}
	}
	return true
}

const cardName = "Untitled" // what every Sony card is called after an in-camera format

type sim struct {
	opts Options
	out  io.Writer

	root, photos, inbox, originals, processed, reports string
	volumes, ejected, backupDir, backupUnplugged       string
	fullMarker, socket, socketDir                      string

	tools    tools
	conn     *sql.DB
	logger   *logging.Logger
	tracker  *activity.Tracker
	pipeline *ingest.Pipeline
	outgest  *outgest.Pipeline
	watcher  *sdcard.Watcher
	server   *ipc.Server
	stop     chan struct{}
	done     chan struct{}

	reportCh  chan db.CardReport
	previewCh chan *cardverify.Preview

	mu         sync.Mutex
	backupPath string

	nextShot, nextClip int
	rng                *rand.Rand
}

// Run builds the sandbox, runs the scenarios in order, and tears down the
// daemon pieces (the sandbox files stay for inspection).
func Run(opts Options) (Summary, error) {
	s := &sim{opts: opts, out: opts.Out, rng: rand.New(rand.NewSource(7))}
	if s.out == nil {
		s.out = io.Discard
	}
	if err := s.setup(); err != nil {
		return Summary{Dir: s.root}, err
	}
	defer s.teardown()

	sum := Summary{Dir: s.root, ReportsDir: s.reports, LogFile: filepath.Join(s.photos, "framelog.log")}
	s.say("Sandbox: %s\n", s.root)
	rcloneKind := "fake"
	if s.tools.realRclone {
		rcloneKind = "real (" + s.tools.rclone + ")"
	}
	s.say("Tools:   git (real), rclone %s, exiftool/diskutil/pmset (fake)\n\n", rcloneKind)

	want := map[string]bool{}
	for _, id := range opts.Only {
		want[id] = true
	}
	last := -1
	for i, sc := range scenarios {
		if want[sc.id] {
			last = i
		}
	}
	for i, sc := range scenarios {
		if len(want) > 0 && i > last {
			break
		}
		r := s.runScenario(i+1, sc)
		sum.Results = append(sum.Results, r)
	}
	return sum, nil
}

func (s *sim) say(format string, args ...any) { fmt.Fprintf(s.out, format, args...) }

func (s *sim) setup() error {
	root := s.opts.Dir
	if root == "" {
		var err error
		if root, err = os.MkdirTemp("", "framelog-sim-"); err != nil {
			return err
		}
	}
	root, _ = filepath.Abs(root)
	s.root = root
	s.photos = filepath.Join(root, "Photos")
	s.inbox = filepath.Join(s.photos, "inbox")
	s.originals = filepath.Join(s.photos, "originals")
	s.processed = filepath.Join(s.photos, "processed")
	s.reports = filepath.Join(s.photos, "reports")
	s.volumes = filepath.Join(root, "Volumes")
	s.ejected = filepath.Join(root, "card-reader-empty")
	s.backupDir = filepath.Join(root, "BackupDrive")
	s.backupUnplugged = filepath.Join(root, "BackupDrive-unplugged")
	s.fullMarker = filepath.Join(root, "backup-drive-is-full")
	for _, d := range []string{s.inbox, s.originals, s.processed, s.volumes, s.ejected, s.backupDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	// A short, separate directory for the socket: macOS caps socket paths at
	// 104 bytes and a user-chosen sandbox path can be long.
	var err error
	if s.socketDir, err = os.MkdirTemp("", "flsim"); err != nil {
		return err
	}
	s.socket = filepath.Join(s.socketDir, "framelog.sock")

	// Tools.
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if s.tools, err = writeFakeTools(binDir, s.backupDir, s.fullMarker); err != nil {
		return err
	}
	if s.tools.git, err = exec.LookPath("git"); err != nil {
		return errors.New("git is needed for the simulation and was not found on PATH")
	}
	if s.opts.RealRclone {
		p, err := backup.FindRclone()
		if err != nil {
			return err
		}
		s.tools.rclone, s.tools.realRclone = p, true
	}

	// Workspace, as the daemon's initWorkspace leaves it.
	if _, err := os.Stat(filepath.Join(s.originals, ".git")); os.IsNotExist(err) {
		if out, err := exec.Command(s.tools.git, "-C", s.originals, "init", "-q").CombinedOutput(); err != nil {
			return fmt.Errorf("git init: %v\n%s", err, out)
		}
		for _, kv := range [][2]string{{"user.email", "sim@framelog.local"}, {"user.name", "Framelog Sim"}} {
			exec.Command(s.tools.git, "-C", s.originals, "config", kv[0], kv[1]).Run() //nolint:errcheck
		}
	}
	ignore := "# managed by framelogd — only XMP sidecars are tracked\n*\n!*/\n!*.xmp\n!.gitignore\n"
	if err := os.WriteFile(filepath.Join(s.originals, ".gitignore"), []byte(ignore), 0o644); err != nil {
		return err
	}

	// Daemon pieces, wired the way cmd/framelogd's mainRun wires them.
	if s.logger, err = logging.New(filepath.Join(s.photos, "framelog.log")); err != nil {
		return err
	}
	s.logger.SetEcho(s.opts.PipelineLog)
	if s.conn, err = db.Open(filepath.Join(s.photos, "catalog.db"), false); err != nil {
		return err
	}
	if err := db.InitDB(s.conn); err != nil {
		return err
	}
	config.SetImportExtensions(nil) //nolint:errcheck — defaults never fail
	s.backupPath = s.backupDir
	s.tracker = &activity.Tracker{}
	s.pipeline = &ingest.Pipeline{
		DB: s.conn, Logger: s.logger, InboxPath: s.inbox, OriginalsPath: s.originals,
		ExiftoolPath: s.tools.exiftool, GitPath: s.tools.git, PmsetPath: s.tools.pmset,
		RclonePath: s.tools.rclone, Activity: s.tracker,
	}
	s.pipeline.SetBackupPath(s.backupDir)
	s.outgest = &outgest.Pipeline{DB: s.conn, Logger: s.logger, ProcessedPath: s.processed,
		ExiftoolPath: s.tools.exiftool}

	s.reportCh = make(chan db.CardReport, 16)
	s.previewCh = make(chan *cardverify.Preview, 16)
	s.watcher = &sdcard.Watcher{
		DiskutilPath: s.tools.diskutil, RclonePath: s.tools.rclone,
		VolumesRoot: s.volumes, InboxPath: s.inbox, PollInterval: 100 * time.Millisecond,
		Runner: s.pipeline, Logger: s.logger, DB: s.conn, OriginalsPath: s.originals,
		ReportsDir: s.reports, BackupPath: s.getBackupPath, Activity: s.tracker,
		OnReport:  func(r db.CardReport) { s.reportCh <- r },
		OnPreview: func(p *cardverify.Preview) { s.previewCh <- p },
	}
	s.server = &ipc.Server{
		SocketPath: s.socket, Ingest: s.pipeline, Outgest: s.outgest,
		Status:  &simStatus{s},
		Config:  &simConfig{s},
		Reports: &simReports{s},
		Card:    s.watcher,
		Backup:  s.pipeline,
		Logger:  s.logger, Version: "sim", ReadDeadline: 5 * time.Second,
		Caps: ipc.Capabilities{SDCardWatch: true, Backup: true, ACPowerGate: true},
	}
	if err := s.server.Start(); err != nil {
		return err
	}

	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		s.watcher.Run(s.stop) //nolint:errcheck
	}()

	s.nextShot, s.nextClip = 1, 1
	return s.makeCard()
}

func (s *sim) teardown() {
	if s.stop != nil {
		close(s.stop)
		<-s.done
	}
	if s.server != nil {
		s.server.Stop() //nolint:errcheck
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.logger != nil {
		s.logger.Close()
	}
	os.RemoveAll(s.socketDir)
	config.SetImportExtensions(nil) //nolint:errcheck
}

func (s *sim) getBackupPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backupPath
}

func (s *sim) setBackupPath(p string) {
	s.mu.Lock()
	s.backupPath = p
	s.mu.Unlock()
	s.pipeline.SetBackupPath(p)
}

// ---- the fake card ----------------------------------------------------------

func (s *sim) cardPath() string    { return filepath.Join(s.ejected, cardName) }
func (s *sim) mountedPath() string { return filepath.Join(s.volumes, cardName) }

// cardDir is wherever the card currently is (in the reader or not).
func (s *sim) cardDir() string {
	if _, err := os.Stat(s.mountedPath()); err == nil {
		return s.mountedPath()
	}
	return s.cardPath()
}

// makeCard lays out a freshly shot α7C II card: RAW+JPEG pairs, HEIF stills,
// two XAVC clips with their XML sidecars, a thumbnail the camera keeps for
// its own playback, and the ._ files a Mac leaves behind.
func (s *sim) makeCard() error {
	for i := 0; i < 2; i++ {
		if err := s.shoot("ARW", "JPG"); err != nil {
			return err
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.shoot("HIF"); err != nil {
			return err
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.film(); err != nil {
			return err
		}
	}
	card := s.cardPath()
	if err := s.write(filepath.Join(card, "PRIVATE/M4ROOT/THMBNL/C0001T01.JPG"), 2_000); err != nil {
		return err
	}
	return s.write(filepath.Join(card, "DCIM/100MSDCF/._DSC00001.ARW"), 4_096)
}

// shoot adds one still (one file per extension, sharing a frame number).
func (s *sim) shoot(exts ...string) error {
	n := s.nextShot
	s.nextShot++
	for _, ext := range exts {
		size := 180_000 + s.rng.Intn(60_000) // > 128 KiB so quick hashes read both ends
		p := filepath.Join(s.cardDir(), fmt.Sprintf("DCIM/100MSDCF/DSC%05d.%s", n, ext))
		if err := s.write(p, size); err != nil {
			return err
		}
	}
	return nil
}

// film adds one XAVC clip plus its NonRealTimeMeta sidecar.
func (s *sim) film() error {
	n := s.nextClip
	s.nextClip++
	dir := filepath.Join(s.cardDir(), "PRIVATE/M4ROOT/CLIP")
	if err := s.write(filepath.Join(dir, fmt.Sprintf("C%04d.MP4", n)), 400_000+s.rng.Intn(100_000)); err != nil {
		return err
	}
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<NonRealTimeMeta xmlns="urn:schemas-professionalDisc:nonRealTimeMeta:ver.2.20">
    <CreationDate value="2026-11-06T18:%02d:10+11:00"/>
    <Device manufacturer="Sony" modelName="ILCE-7CM2" serialNo="01380224"/>
    <Lens modelName="FE 40mm F2.5 G"/>
</NonRealTimeMeta>
`, 40+n)
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("C%04dM01.XML", n)), []byte(xml), 0o644)
}

// write creates a file of random bytes (unique per file, like real photos).
func (s *sim) write(path string, size int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b := make([]byte, size)
	s.rng.Read(b)
	return os.WriteFile(path, b, 0o644)
}

// ---- inserting and ejecting -------------------------------------------------

// insert moves the card into Volumes/ and waits for the watcher to finish
// the whole run (preview, copy, import, backup, verify).
func (s *sim) insert() (*cardverify.Preview, db.CardReport, error) {
	s.drainReports()
	if err := os.Rename(s.cardPath(), s.mountedPath()); err != nil {
		return nil, db.CardReport{}, fmt.Errorf("insert card: %w", err)
	}
	var preview *cardverify.Preview
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case p := <-s.previewCh:
			preview = p
		case r := <-s.reportCh:
			// The preview is always sent before the report.
			select {
			case p := <-s.previewCh:
				preview = p
			default:
			}
			return preview, r, nil
		case <-timeout:
			return preview, db.CardReport{}, errors.New("no card report within 2 minutes")
		}
	}
}

// drainReports discards queued reports and previews so the next wait only
// sees what the next action produces.
func (s *sim) drainReports() {
	for {
		select {
		case <-s.reportCh:
		case <-s.previewCh:
		default:
			return
		}
	}
}

// eject moves the card out of Volumes/ and waits long enough for the
// watcher to notice, so the next insert counts as a new insertion.
func (s *sim) eject() error {
	if _, err := os.Stat(s.mountedPath()); err != nil {
		return nil // already out
	}
	if err := os.Rename(s.mountedPath(), s.cardPath()); err != nil {
		return fmt.Errorf("eject card: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

// ---- the socket, as the menu bar app sees it --------------------------------

func (s *sim) call(payload string) (map[string]any, error) {
	conn, err := net.DialTimeout("unix", s.socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, payload); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal([]byte(line), &m)
}

func (s *sim) command(name string) (map[string]any, error) {
	return s.call(fmt.Sprintf(`{"command":%q}`, name))
}

// ---- ipc adapters (the daemon's live in cmd/framelogd) -----------------------

type simStatus struct{ s *sim }

func (a *simStatus) IngestRunning() bool         { return a.s.pipeline.IngestRunning() }
func (a *simStatus) OutgestRunning() bool        { return a.s.outgest.OutgestRunning() }
func (a *simStatus) PhotoCount() (int, error)    { return db.PhotoCount(a.s.conn) }
func (a *simStatus) LastImport() (string, error) { return db.LastImport(a.s.conn) }
func (a *simStatus) BackupDriveMounted() bool    { return backup.IsDriveMounted(a.s.getBackupPath()) }
func (a *simStatus) BackupConfigured() bool      { return a.s.getBackupPath() != "" }
func (a *simStatus) Paused() bool                { return a.s.pipeline.Paused() }

type simConfig struct{ s *sim }

func (c *simConfig) SetBackupPath(p string) error { c.s.setBackupPath(p); return nil }
func (c *simConfig) SetImportTypes(types []string) error {
	return config.SetImportExtensions(types)
}

type simReports struct{ s *sim }

func (r *simReports) Activity() activity.Snapshot       { return r.s.tracker.Snapshot() }
func (r *simReports) LastCard() (*db.CardReport, error) { return db.LastCardReport(r.s.conn) }
func (r *simReports) CardPreview() *cardverify.Preview  { return r.s.watcher.CurrentPreview() }
func (r *simReports) Backup() (ipc.BackupSummary, error) {
	sum := ipc.BackupSummary{Pending: r.s.pipeline.BackupPending()}
	last, err := db.LastBackupRun(r.s.conn)
	if err != nil || last == nil {
		return sum, err
	}
	sum.LastOutcome, sum.LastError, sum.LastRunAt = last.Outcome, last.Error, last.FinishedAt
	sum.FilesCopied, sum.FilesFailed = last.FilesCopied, last.FilesFailed
	sum.LastSuccessAt, err = db.LastSuccessfulBackupAt(r.s.conn)
	return sum, err
}

// ---- small helpers for scenarios ---------------------------------------------

// libraryFile returns the imported path of some photo with this extension.
func (s *sim) libraryFile(ext string) (string, error) {
	var p string
	err := s.conn.QueryRow(
		"SELECT imported_path FROM photos WHERE imported_path LIKE ? ORDER BY imported_path LIMIT 1",
		"%"+ext).Scan(&p)
	return p, err
}

// inboxLeftovers counts files still in inbox/ (outside duplicates/ and failed/).
func (s *sim) inboxLeftovers() int {
	n := 0
	filepath.Walk(s.inbox, func(p string, fi os.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if fi.IsDir() && p != s.inbox {
			return filepath.SkipDir
		}
		if !fi.IsDir() && config.IsImportable(filepath.Ext(p)) {
			n++
		}
		return nil
	})
	return n
}

// duplicatesParked counts files ingest set aside as already imported.
func (s *sim) duplicatesParked() int {
	entries, _ := os.ReadDir(filepath.Join(s.inbox, config.DuplicatesDirName))
	return len(entries)
}

func problemsText(r db.CardReport) string { return strings.Join(r.Problems, " | ") }

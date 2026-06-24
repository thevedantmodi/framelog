// Package main is the framelogd daemon — the Go side of Framelog (FL-304).
// It wires together all Phase 1–3 packages and exposes three subcommands:
//
//	framelogd run      – start the daemon loop (this is what launchd runs)
//	framelogd install  – write the launchd plist and bootstrap the agent
//	framelogd uninstall – bootout the agent and remove the plist
//	framelogd --version – print the build version and exit
//
// install/uninstall are EXPLICIT subcommands — they never fire as a side effect
// of a bare `framelogd` invocation. Auto-installing a launchd agent as a side
// effect of every `go run` or manual test invocation would be a footgun.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thevedantmodi/framelog/core/backup"
	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/db"
	"github.com/thevedantmodi/framelog/core/exif"
	"github.com/thevedantmodi/framelog/core/gitops"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/ipc"
	"github.com/thevedantmodi/framelog/core/launchd"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
	"github.com/thevedantmodi/framelog/core/outgestwatcher"
	"github.com/thevedantmodi/framelog/core/sdcard"
	"github.com/thevedantmodi/framelog/core/triggerwatcher"
	"github.com/thevedantmodi/framelog/core/xmpwatcher"
)

// Version is stamped by the build system:
//
//	go build -ldflags "-X main.Version=$(cat ../../VERSION)"
//
// Stays "dev" for `go run` and untagged test builds.
var Version = "dev"

// plistPath is where Install writes the launchd agent plist.
func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchd.Label+".plist")
}

// initWorkspace creates inbox/, originals/, and processed/ if they do not exist,
// and runs `git init` inside originals/ the first time (when originals/.git is
// absent). Idempotent — safe to call on every daemon startup.
func initWorkspace(inbox, originals, processed, gitPath string) error {
	for _, dir := range []string{inbox, originals, processed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("initWorkspace: mkdir %s: %w", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(originals, ".git")); os.IsNotExist(err) {
		out, err := exec.Command(gitPath, "-C", originals, "init").CombinedOutput()
		if err != nil {
			return fmt.Errorf("initWorkspace: git init %s: %w\n%s", originals, err, out)
		}
	}

	// Only track .xmp sidecars — photo bytes are large binaries that make
	// git add -A slow (git must SHA-1 every byte). The photos themselves are
	// safe in originals/; git is only here to version the XMP edits.
	gitignore := filepath.Join(originals, ".gitignore")
	if _, err := os.Stat(gitignore); os.IsNotExist(err) {
		ignore := "# managed by framelogd — only XMP sidecars are tracked\n*\n!*/\n!*.xmp\n!.gitignore\n"
		if err := os.WriteFile(gitignore, []byte(ignore), 0o644); err != nil {
			return fmt.Errorf("initWorkspace: write .gitignore: %w", err)
		}
	}

	return nil
}

func main() {
	args := os.Args[1:]

	if len(args) == 0 || args[0] == "run" {
		if err := mainRun(); err != nil {
			fmt.Fprintf(os.Stderr, "framelogd: %v\n", err)
			os.Exit(1)
		}
		return
	}

	switch args[0] {
	case "--version", "-version", "version":
		fmt.Println(Version)

	case "install":
		// Parse optional --remote <url> flag.
		var remoteURL string
		installArgs := args[1:]
		for i := 0; i < len(installArgs); i++ {
			if installArgs[i] == "--remote" && i+1 < len(installArgs) {
				remoteURL = strings.TrimSpace(installArgs[i+1])
				i++
			}
		}

		launchctlPath, err := launchd.FindLaunchctl()
		if err != nil {
			fmt.Fprintf(os.Stderr, "framelogd install: %v\n", err)
			os.Exit(1)
		}
		execPath, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "framelogd install: %v\n", err)
			os.Exit(1)
		}
		home, _ := os.UserHomeDir()

		gitPath, err := gitops.FindGit()
		if err != nil {
			fmt.Fprintf(os.Stderr, "framelogd install: %v\n", err)
			os.Exit(1)
		}
		if err := initWorkspace(config.Inbox, config.Originals, config.Processed, gitPath); err != nil {
			fmt.Fprintf(os.Stderr, "framelogd install: %v\n", err)
			os.Exit(1)
		}

		if remoteURL != "" {
			if ok, _ := gitops.HasRemote(gitPath, config.Originals); !ok {
				out, err := exec.Command(gitPath, "-C", config.Originals, "remote", "add", "origin", remoteURL).CombinedOutput()
				if err != nil {
					fmt.Fprintf(os.Stderr, "framelogd install: git remote add: %v\n%s", err, out)
					os.Exit(1)
				}
			}
		}

		if err := launchd.Install(launchctlPath, plistPath(), execPath, home); err != nil {
			fmt.Fprintf(os.Stderr, "framelogd install: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("framelogd: installed and bootstrapped")

	case "uninstall":
		launchctlPath, err := launchd.FindLaunchctl()
		if err != nil {
			fmt.Fprintf(os.Stderr, "framelogd uninstall: %v\n", err)
			os.Exit(1)
		}
		if err := launchd.Uninstall(launchctlPath, plistPath()); err != nil {
			fmt.Fprintf(os.Stderr, "framelogd uninstall: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("framelogd: uninstalled")

	default:
		fmt.Fprintf(os.Stderr, "usage: framelogd [run|install|uninstall|--version]\n")
		os.Exit(1)
	}
}

// runConfig holds injected dependencies for the daemon loop. Having an explicit
// struct (instead of reading globals in run) makes the daemon fully testable.
type runConfig struct {
	dbConn          *sql.DB
	logger          *logging.Logger
	ingestPipeline  *ingest.Pipeline
	outgestPipeline *outgest.Pipeline
	ipcServer       *ipc.Server
	triggerWatcher  *triggerwatcher.Watcher
	xmpW            *xmpwatcher.Watcher
	outgestW        *outgestwatcher.Watcher
	sdcardW         *sdcard.Watcher // nil when diskutil not found
}

// statusProvider bridges ipc.StatusProvider across the package-boundary gap.
// All four packages (ingest, outgest, db, backup) are imported in main, so
// the wrapper naturally lives here rather than in the ipc package.
type statusProvider struct {
	ingestPipeline  *ingest.Pipeline
	outgestPipeline *outgest.Pipeline
	dbConn          *sql.DB
	backupPath      string
}

func (s *statusProvider) IngestRunning() bool { return s.ingestPipeline.IngestRunning() }
func (s *statusProvider) OutgestRunning() bool {
	return s.outgestPipeline.OutgestRunning()
}
func (s *statusProvider) PhotoCount() (int, error)   { return db.PhotoCount(s.dbConn) }
func (s *statusProvider) LastImport() (string, error) { return db.LastImport(s.dbConn) }
func (s *statusProvider) BackupDriveMounted() bool {
	return backup.IsDriveMounted(s.backupPath)
}

// mainRun resolves all binaries, opens the DB, wires the pipeline, and runs
// until SIGINT/SIGTERM.
func mainRun() error {
	// --- Required binaries (fatal if absent) ---
	exiftoolPath, err := exif.FindExiftool()
	if err != nil {
		return fmt.Errorf("required binary not found: %w", err)
	}
	gitPath, err := gitops.FindGit()
	if err != nil {
		return fmt.Errorf("required binary not found: %w", err)
	}

	// --- Optional binaries (degrade gracefully) ---
	pmsetPath, _ := gitops.FindPmset()
	pgrepPath, _ := xmpwatcher.FindPgrep()
	diskutilPath, diskutilErr := sdcard.FindDiskutil()
	rclonePath, _ := backup.FindRclone()

	// --- Logger ---
	if err := os.MkdirAll(filepath.Dir(config.LogFile), 0o755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	logger, err := logging.New(config.LogFile)
	if err != nil {
		return fmt.Errorf("logging.New: %w", err)
	}
	defer logger.Close()

	logger.Log(logging.PrefixCore, fmt.Sprintf("framelogd %s starting", Version))
	logger.Log(logging.PrefixCore, fmt.Sprintf("exiftool: %s", exiftoolPath))
	logger.Log(logging.PrefixCore, fmt.Sprintf("git: %s", gitPath))

	// Log optional binary status so the operator can verify degraded-mode decisions
	// without guessing. Each "not found" case documents the specific capability lost.
	if pmsetPath != "" {
		logger.Log(logging.PrefixCore, fmt.Sprintf("pmset: %s", pmsetPath))
	} else {
		logger.Log(logging.PrefixCore, "pmset not found — git push not gated on AC power (will always attempt)")
	}
	if pgrepPath != "" {
		logger.Log(logging.PrefixCore, fmt.Sprintf("pgrep: %s", pgrepPath))
	} else {
		logger.Log(logging.PrefixCore, "pgrep not found — Lightroom-running check skipped (XMP commits will always push)")
	}
	if diskutilErr == nil {
		logger.Log(logging.PrefixCore, fmt.Sprintf("diskutil: %s", diskutilPath))
	} else {
		logger.Log(logging.PrefixCore, "diskutil not found — SD card watcher disabled")
	}
	if rclonePath != "" {
		logger.Log(logging.PrefixCore, fmt.Sprintf("rclone: %s", rclonePath))
	} else {
		logger.Log(logging.PrefixCore, "rclone not found — backup disabled")
	}

	// --- Work directories + git repo ---
	if err := initWorkspace(config.Inbox, config.Originals, config.Processed, gitPath); err != nil {
		return err
	}

	// --- Database ---
	dbConn, err := db.Open(config.DBPath, false)
	if err != nil {
		return fmt.Errorf("db.Open: %w", err)
	}
	defer dbConn.Close()
	if err := db.InitDB(dbConn); err != nil {
		return fmt.Errorf("db.InitDB: %w", err)
	}

	// --- Pipelines ---
	ingestPipeline := &ingest.Pipeline{
		DB:            dbConn,
		Logger:        logger,
		InboxPath:     config.Inbox,
		OriginalsPath: config.Originals,
		ExiftoolPath:  exiftoolPath,
		GitPath:       gitPath,
		PmsetPath:     pmsetPath,
		RclonePath:    rclonePath,
		BackupPath:    config.BackupPath,
	}
	outgestPipeline := &outgest.Pipeline{
		DB:            dbConn,
		Logger:        logger,
		ProcessedPath: config.Processed,
		ExiftoolPath:  exiftoolPath,
	}

	// --- IPC server ---
	sp := &statusProvider{
		ingestPipeline:  ingestPipeline,
		outgestPipeline: outgestPipeline,
		dbConn:          dbConn,
		backupPath:      config.BackupPath,
	}
	ipcServer := &ipc.Server{
		SocketPath:   config.SocketPath,
		Ingest:       ingestPipeline,
		Outgest:      outgestPipeline,
		Status:       sp,
		Logger:       logger,
		ReadDeadline: 5 * time.Second,
	}

	// --- Trigger watcher ---
	tw := &triggerwatcher.Watcher{
		IngestTriggerPath:  config.IngestTrigger,
		OutgestTriggerPath: config.OutgestTrigger,
		PollInterval:       2 * time.Second,
		Ingest:             ingestPipeline,
		Outgest:            outgestPipeline,
		Logger:             logger,
	}

	// --- XMP watcher ---
	xmpW := &xmpwatcher.Watcher{
		GitPath:          gitPath,
		ExiftoolPath:     exiftoolPath,
		OriginalsPath:    config.Originals,
		PmsetPath:        pmsetPath,
		PgrepPath:        pgrepPath,
		DB:               dbConn,
		Logger:           logger,
		DebounceDuration: time.Duration(config.DebounceSeconds) * time.Second,
	}

	// --- Outgest watcher ---
	outgestW := &outgestwatcher.Watcher{
		ProcessedPath:    config.Processed,
		Outgest:          outgestPipeline,
		Logger:           logger,
		DebounceDuration: time.Duration(config.DebounceSeconds) * time.Second,
	}

	// --- SD card watcher (optional) ---
	var sdcardW *sdcard.Watcher
	if diskutilErr == nil {
		sdcardW = &sdcard.Watcher{
			DiskutilPath: diskutilPath,
			VolumesRoot:  "/Volumes",
			InboxPath:    config.Inbox,
			Runner:       ingestPipeline,
			Logger:       logger,
		}
	}

	rc := &runConfig{
		dbConn:          dbConn,
		logger:          logger,
		ingestPipeline:  ingestPipeline,
		outgestPipeline: outgestPipeline,
		ipcServer:       ipcServer,
		triggerWatcher:  tw,
		xmpW:            xmpW,
		outgestW:        outgestW,
		sdcardW:         sdcardW,
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		logger.Log(logging.PrefixCore, "received signal, shutting down")
		close(stop)
	}()

	return run(rc, stop)
}

// run starts all watchers and the IPC server and blocks until stop is closed.
// Exported for testing via runConfig injection.
func run(rc *runConfig, stop <-chan struct{}) error {
	if err := rc.ipcServer.Start(); err != nil {
		return fmt.Errorf("ipc server: %w", err)
	}
	defer rc.ipcServer.Stop()

	twStop := make(chan struct{})
	go rc.triggerWatcher.Run(twStop)

	xmpErrCh := make(chan error, 1)
	go func() { xmpErrCh <- rc.xmpW.Run() }()

	outgestErrCh := make(chan error, 1)
	go func() { outgestErrCh <- rc.outgestW.Run() }()

	if rc.sdcardW != nil {
		go rc.sdcardW.Run(stop)
	}

	<-stop

	close(twStop)
	rc.xmpW.Stop()
	rc.outgestW.Stop()

	return nil
}

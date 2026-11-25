package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/config"
	"github.com/thevedantmodi/framelog/core/ingest"
	"github.com/thevedantmodi/framelog/core/ipc"
	"github.com/thevedantmodi/framelog/core/logging"
	"github.com/thevedantmodi/framelog/core/outgest"
	"github.com/thevedantmodi/framelog/core/outgestwatcher"
	"github.com/thevedantmodi/framelog/core/triggerwatcher"
)

// useTempUserConfig points config.UserConfigPath at a fresh temp file.
func useTempUserConfig(t *testing.T) {
	t.Helper()
	orig := config.UserConfigPath
	config.UserConfigPath = filepath.Join(t.TempDir(), "framelog_config.json")
	t.Cleanup(func() { config.UserConfigPath = orig })
}

// With XMP sync off, initWorkspace creates the directories but no git repo
// and no .gitignore — git need not even be installed (gitPath is bogus).
func TestInitWorkspace_XMPSyncOffSkipsGit(t *testing.T) {
	base := t.TempDir()
	inbox, originals, processed := filepath.Join(base, "inbox"), filepath.Join(base, "originals"), filepath.Join(base, "processed")

	if err := initWorkspace(inbox, originals, processed, "/nonexistent/git", false); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}
	for _, d := range []string{inbox, originals, processed} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s not created: %v", d, err)
		}
	}
	for _, f := range []string{".git", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(originals, f)); err == nil {
			t.Errorf("originals/%s created with XMP sync off", f)
		}
	}
}

// run() must start and stop cleanly with no XMP watcher (XMP sync off).
func TestRun_NilXMPWatcher(t *testing.T) {
	base := shortTempDir(t)
	originals, processed := filepath.Join(base, "originals"), filepath.Join(base, "processed")
	for _, d := range []string{originals, processed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logger, err := logging.New(filepath.Join(base, "test.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	ingestP := &ingest.Pipeline{Logger: logger, OriginalsPath: originals}
	outgestP := &outgest.Pipeline{Logger: logger, ProcessedPath: processed}
	rc := &runConfig{
		logger: logger, ingestPipeline: ingestP, outgestPipeline: outgestP,
		ipcServer: &ipc.Server{
			SocketPath: filepath.Join(base, "d.sock"), Ingest: ingestP, Outgest: outgestP,
			Status: &statusProvider{ingestPipeline: ingestP, outgestPipeline: outgestP, configSetter: &configSetter{pipeline: ingestP}},
			Logger: logger, ReadDeadline: 2 * time.Second,
		},
		triggerWatcher: &triggerwatcher.Watcher{
			IngestTriggerPath:  filepath.Join(base, ".ingest_trigger"),
			OutgestTriggerPath: filepath.Join(base, ".outgest_trigger"),
			PollInterval:       50 * time.Millisecond,
			Ingest:             ingestP, Outgest: outgestP, Logger: logger,
		},
		outgestW: &outgestwatcher.Watcher{
			ProcessedPath: processed, Outgest: outgestP, Logger: logger,
			DebounceDuration: 50 * time.Millisecond,
		},
	}

	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- run(rc, stop) }()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run() with nil xmpW: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return after stop")
	}
}

func TestXMPSyncCmd_TogglesPersistedSetting(t *testing.T) {
	useTempUserConfig(t)
	if err := config.UpdateUserConfig(func(u *config.UserConfig) { u.BackupPath = "/Volumes/B" }); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		arg  string
		want bool
	}{{"on", true}, {"off", false}} {
		if err := xmpSyncCmd([]string{tc.arg}); err != nil {
			t.Fatalf("xmp-sync %s: %v", tc.arg, err)
		}
		cfg, err := config.ReadUserConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.XMPSync != tc.want {
			t.Errorf("after %s: XMPSync = %v", tc.arg, cfg.XMPSync)
		}
		if cfg.BackupPath != "/Volumes/B" {
			t.Errorf("after %s: backup path reset to %q", tc.arg, cfg.BackupPath)
		}
	}
	if err := xmpSyncCmd([]string{"maybe"}); err == nil {
		t.Error("xmp-sync maybe: want error")
	}
}

func TestCleanXMPCmd(t *testing.T) {
	useTempUserConfig(t)
	originals := t.TempDir()
	day := filepath.Join(originals, "2026", "06", "22")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	photo, sidecar := filepath.Join(day, "a.raf"), filepath.Join(day, "a.xmp")
	for _, f := range []string{photo, sidecar} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Refused while XMP sync is on: the watcher would commit the deletions.
	if err := config.UpdateUserConfig(func(u *config.UserConfig) { u.XMPSync = true }); err != nil {
		t.Fatal(err)
	}
	if err := cleanXMPCmd(nil, originals); err == nil || !strings.Contains(err.Error(), "xmp-sync off") {
		t.Errorf("with sync on: err = %v, want refusal", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatal("sidecar removed while sync on")
	}

	if err := config.UpdateUserConfig(func(u *config.UserConfig) { u.XMPSync = false }); err != nil {
		t.Fatal(err)
	}
	if err := cleanXMPCmd([]string{"--bogus"}, originals); err == nil {
		t.Error("unknown flag: want error")
	}
	if err := cleanXMPCmd([]string{"--dry-run"}, originals); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatal("dry run removed the sidecar")
	}
	if err := cleanXMPCmd(nil, originals); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(sidecar); err == nil {
		t.Error("sidecar still present after clean")
	}
	if _, err := os.Stat(photo); err != nil {
		t.Errorf("photo removed: %v", err)
	}
}

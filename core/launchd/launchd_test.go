package launchd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thevedantmodi/framelog/core/config"
)

// fakeLaunchctl writes a shell script to dir that appends its args to a log
// file and exits 0. Returns the script path.
func fakeLaunchctl(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "launchctl")
	content := `#!/bin/sh
echo "$@" >> "` + filepath.Join(dir, "launchctl.log") + `"
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake launchctl: %v", err)
	}
	return script
}

// readLog reads the fake launchctl invocation log.
func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "launchctl.log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("readLog: %v", err)
	}
	return string(b)
}

// ---- GeneratePlist tests ----------------------------------------------------

func TestGeneratePlist_ContainsLabel(t *testing.T) {
	b, err := GeneratePlist("/usr/local/bin/framelogd", "/Users/test")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	plist := string(b)
	if !strings.Contains(plist, Label) {
		t.Errorf("plist does not contain label %q", Label)
	}
}

func TestGeneratePlist_ContainsExecPath(t *testing.T) {
	execPath := "/usr/local/bin/framelogd"
	b, err := GeneratePlist(execPath, "/Users/test")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	if !strings.Contains(string(b), execPath) {
		t.Errorf("plist does not contain execPath %q", execPath)
	}
}

func TestGeneratePlist_RunAtLoadAndKeepAliveTrue(t *testing.T) {
	b, err := GeneratePlist("/usr/local/bin/framelogd", "/Users/test")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	plist := string(b)
	if !strings.Contains(plist, "RunAtLoad") {
		t.Error("plist missing RunAtLoad key")
	}
	if !strings.Contains(plist, "KeepAlive") {
		t.Error("plist missing KeepAlive key")
	}
}

func TestGeneratePlist_UsesCrashLogPathNotLogFile(t *testing.T) {
	homeDir := "/Users/testuser"
	b, err := GeneratePlist("/usr/local/bin/framelogd", homeDir)
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	plist := string(b)

	// CRITICAL: config.LogFile must NOT appear anywhere in the plist.
	// CrashLogPath and LogFile are intentionally different paths.
	// If this fails, launchd would double-write every structured log line.
	if strings.Contains(plist, config.LogFile) {
		t.Errorf("plist contains config.LogFile %q — must use CrashLogPath instead\n"+
			"See CrashLogPath design note in config.go and launchd.go for rationale",
			config.LogFile)
	}

	// Verify CrashLogPath is actually present (the correct path).
	expectedCrashLog := filepath.Join(homeDir, "Library", "Logs", "Framelog", "crash.log")
	if !strings.Contains(plist, expectedCrashLog) {
		t.Errorf("plist does not contain expected CrashLogPath %q", expectedCrashLog)
	}
}

func TestGeneratePlist_StandardOutAndErrBothPresent(t *testing.T) {
	b, err := GeneratePlist("/usr/local/bin/framelogd", "/Users/test")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	plist := string(b)
	if !strings.Contains(plist, "StandardOutPath") {
		t.Error("plist missing StandardOutPath key")
	}
	if !strings.Contains(plist, "StandardErrorPath") {
		t.Error("plist missing StandardErrorPath key")
	}
}

func TestGeneratePlist_RunSubcommand(t *testing.T) {
	b, err := GeneratePlist("/usr/local/bin/framelogd", "/Users/test")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}
	// Must pass "run" subcommand so launchd starts the daemon loop, not install.
	if !strings.Contains(string(b), "<string>run</string>") {
		t.Error("plist ProgramArguments does not include 'run' subcommand")
	}
}

// ---- Install / Uninstall tests ----------------------------------------------

func TestInstall_WritesPlistAndCallsBootstrap(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctl(t, dir)
	plistPath := filepath.Join(dir, "Library", "LaunchAgents", "com.framelog.core.plist")

	err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Plist file must exist.
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("plist not written: %v", err)
	}

	// Crash log directory must be created.
	crashLogDir := filepath.Join(dir, "Library", "Logs", "Framelog")
	if _, err := os.Stat(crashLogDir); err != nil {
		t.Errorf("crash log dir not created: %v", err)
	}

	// Fake launchctl log should contain "bootstrap" invocation.
	log := readLog(t, dir)
	if !strings.Contains(log, "bootstrap") {
		t.Errorf("launchctl log does not contain 'bootstrap'; got: %q", log)
	}
}

func TestInstall_IdemPotent_BootoutCalledFirst(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctl(t, dir)
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	// Install twice — second install should not error.
	for i := 0; i < 2; i++ {
		if err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir); err != nil {
			t.Fatalf("Install round %d: %v", i+1, err)
		}
	}

	log := readLog(t, dir)
	if !strings.Contains(log, "bootout") {
		t.Errorf("launchctl log does not contain 'bootout'; got: %q", log)
	}
}

func TestUninstall_CallsBootoutAndRemovesPlist(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctl(t, dir)
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	// Write a dummy plist so Remove has something to do.
	os.WriteFile(plistPath, []byte("dummy"), 0o644)

	if err := Uninstall(fake, plistPath); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(plistPath); err == nil {
		t.Error("plist still exists after Uninstall")
	}

	log := readLog(t, dir)
	if !strings.Contains(log, "bootout") {
		t.Errorf("launchctl log does not contain 'bootout'; got: %q", log)
	}
}

func TestUninstall_NoPlistFile_NoError(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctl(t, dir)
	plistPath := filepath.Join(dir, "nonexistent.plist")

	// Uninstalling when plist doesn't exist must not error.
	if err := Uninstall(fake, plistPath); err != nil {
		t.Fatalf("Uninstall without plist: %v", err)
	}
}

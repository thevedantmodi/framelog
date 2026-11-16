package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thevedantmodi/framelog/core/config"
)

// fakeOpts tunes the stateful fake launchctl to reproduce real launchd quirks.
type fakeOpts struct {
	// bootoutLinger: number of `print` calls after bootout that still report
	// the job as loaded (bootout is asynchronous on real macOS).
	bootoutLinger int
	// bootstrapFailures: number of initial `bootstrap` calls that fail.
	bootstrapFailures int
	// noRunAtLoad: bootstrap loads the job but does not start it (observed
	// on real macOS: loaded, runs = 0) — only kickstart starts it.
	noRunAtLoad bool
	// neverRuns: kickstart succeeds but the job never reaches running.
	neverRuns bool
}

// fakeLaunchctl writes a shell script to dir that appends its args to a log
// file and simulates launchd job state with marker files. Returns the path.
func fakeLaunchctl(t *testing.T, dir string) string {
	t.Helper()
	return fakeLaunchctlWith(t, dir, fakeOpts{})
}

func fakeLaunchctlWith(t *testing.T, dir string, o fakeOpts) string {
	t.Helper()
	shrinkTimings(t)
	q := func(name string) string { return `"` + filepath.Join(dir, name) + `"` }
	writeCount := func(name string, n int) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprint(n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCount("bootstrap_failures", o.bootstrapFailures)
	runAtLoad := "touch " + q("running")
	if o.noRunAtLoad || o.neverRuns {
		runAtLoad = ":"
	}
	kickstart := `[ -f ` + q("loaded") + ` ] && touch ` + q("running")
	if o.neverRuns {
		kickstart = ":"
	}
	script := filepath.Join(dir, "launchctl")
	content := `#!/bin/sh
echo "$@" >> ` + q("launchctl.log") + `
case "$1" in
bootout)
	rm -f ` + q("running") + `
	if [ -f ` + q("loaded") + ` ]; then echo ` + fmt.Sprint(o.bootoutLinger) + ` > ` + q("linger") + `; fi
	exit 0 ;;
bootstrap)
	n=$(cat ` + q("bootstrap_failures") + `)
	if [ "$n" -gt 0 ]; then echo $((n-1)) > ` + q("bootstrap_failures") + `; echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi
	touch ` + q("loaded") + `
	` + runAtLoad + `
	exit 0 ;;
kickstart)
	` + kickstart + `
	exit 0 ;;
print)
	if [ -f ` + q("linger") + ` ]; then
		n=$(cat ` + q("linger") + `)
		if [ "$n" -gt 0 ]; then echo $((n-1)) > ` + q("linger") + `; echo "state = not running"; exit 0; fi
		rm -f ` + q("linger") + ` ` + q("loaded") + `
	fi
	[ -f ` + q("loaded") + ` ] || { echo "Could not find service" >&2; exit 113; }
	if [ -f ` + q("running") + ` ]; then echo "	state = running"; else echo "	state = not running"; fi
	exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake launchctl: %v", err)
	}
	return script
}

// shrinkTimings makes Install's polling fast for tests and restores it after.
func shrinkTimings(t *testing.T) {
	t.Helper()
	pi, bt, st, ba := pollInterval, bootoutTimeout, startTimeout, bootstrapAttempts
	pollInterval, bootoutTimeout, startTimeout, bootstrapAttempts =
		time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, 5
	t.Cleanup(func() {
		pollInterval, bootoutTimeout, startTimeout, bootstrapAttempts = pi, bt, st, ba
	})
}

// commands returns the launchctl subcommands invoked, in order.
func commands(t *testing.T, dir string) []string {
	t.Helper()
	var cmds []string
	for _, line := range strings.Split(strings.TrimSpace(readLog(t, dir)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			cmds = append(cmds, f[0])
		}
	}
	return cmds
}

func indexOf(cmds []string, want string) int {
	for i, c := range cmds {
		if c == want {
			return i
		}
	}
	return -1
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

// Regression: 0.16.0's menu bar auto-ran install over a running 0.15.0
// daemon; bootstrap raced the asynchronous bootout and the daemon never
// came back, so exports sat unsorted in processed/.
func TestInstall_WaitsForBootoutBeforeBootstrap(t *testing.T) {
	dir := t.TempDir()
	plistPath := filepath.Join(dir, "com.framelog.core.plist")
	fake := fakeLaunchctlWith(t, dir, fakeOpts{bootoutLinger: 3})

	if err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	os.Remove(filepath.Join(dir, "launchctl.log"))

	if err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir); err != nil {
		t.Fatalf("reinstall over running job: %v", err)
	}
	cmds := commands(t, dir)
	bootout, bootstrap := indexOf(cmds, "bootout"), indexOf(cmds, "bootstrap")
	if bootout < 0 || bootstrap < 0 {
		t.Fatalf("want bootout and bootstrap, got %v", cmds)
	}
	prints := 0
	for _, c := range cmds[bootout:bootstrap] {
		if c == "print" {
			prints++
		}
	}
	if prints < 4 { // 3 lingering + 1 "not found"
		t.Errorf("bootstrap ran before old job was gone; commands: %v", cmds)
	}
	if !isRunning(fake, "gui/x/"+Label) {
		t.Error("job not running after reinstall")
	}
}

func TestInstall_RetriesFailedBootstrap(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctlWith(t, dir, fakeOpts{bootstrapFailures: 2})
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	if err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	n := 0
	for _, c := range commands(t, dir) {
		if c == "bootstrap" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("bootstrap called %d times, want 3", n)
	}
}

func TestInstall_BootstrapAlwaysFails_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctlWith(t, dir, fakeOpts{bootstrapFailures: 100})
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("want bootstrap error with launchctl output, got %v", err)
	}
}

// Observed on macOS: bootstrap succeeds, RunAtLoad=true, yet runs = 0.
func TestInstall_KickstartsJobLaunchdDidNotStart(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctlWith(t, dir, fakeOpts{noRunAtLoad: true})
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	if err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	cmds := commands(t, dir)
	if k := indexOf(cmds, "kickstart"); k < indexOf(cmds, "bootstrap") {
		t.Errorf("kickstart missing or before bootstrap: %v", cmds)
	}
	if !isRunning(fake, "gui/x/"+Label) {
		t.Error("job not running after Install")
	}
}

func TestInstall_JobNeverRuns_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	fake := fakeLaunchctlWith(t, dir, fakeOpts{neverRuns: true})
	plistPath := filepath.Join(dir, "com.framelog.core.plist")

	err := Install(fake, plistPath, "/usr/local/bin/framelogd", dir)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want not-running error, got %v", err)
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

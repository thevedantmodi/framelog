// Package launchd generates, installs, and uninstalls the com.framelog.core
// launchd agent (FL-303). All launchctl invocations accept an explicit path
// so tests can substitute a fake script without PATH manipulation.
package launchd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"text/template"
	"time"
)

// Label is the launchd job label used in both the plist and launchctl commands.
const Label = "com.framelog.core"

// FindLaunchctl returns the path to launchctl. Checks /bin/launchctl first
// (the canonical macOS location), then falls back to PATH lookup.
func FindLaunchctl() (string, error) {
	const canonical = "/bin/launchctl"
	if _, err := os.Stat(canonical); err == nil {
		return canonical, nil
	}
	path, err := exec.LookPath("launchctl")
	if err != nil {
		return "", fmt.Errorf("launchctl not found: %w", err)
	}
	return path, nil
}

// plistData is the template data for GeneratePlist.
type plistData struct {
	Label        string
	ExecPath     string
	LogDir       string
	CrashLogPath string
	RunAtLoad    bool
	KeepAlive    bool
}

var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.ExecPath}}</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	{{if .RunAtLoad}}<true/>{{else}}<false/>{{end}}
	<key>KeepAlive</key>
	{{if .KeepAlive}}<true/>{{else}}<false/>{{end}}
	<key>StandardOutPath</key>
	<string>{{.CrashLogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.CrashLogPath}}</string>
</dict>
</plist>
`))

// GeneratePlist renders the launchd plist XML for the framelogd daemon.
// execPath is the absolute path to the framelogd binary; homeDir is the user's
// home directory used to derive StandardOutPath/StandardErrorPath.
//
// Both StandardOutPath and StandardErrorPath point at CrashLogPath (inside
// ~/Library/Logs/Framelog/), NOT at config.LogFile (~/.Photos/framelog.log).
// The distinction is intentional: logging.Logger writes directly to LogFile and
// also to stdout, so capturing stdout into LogFile would double every line.
// CrashLogPath only receives output that bypasses logging.Logger entirely —
// uncaught runtime panics (written to real stderr) or failures before logging
// starts.
func GeneratePlist(execPath, homeDir string) ([]byte, error) {
	data := plistData{
		Label:        Label,
		ExecPath:     execPath,
		CrashLogPath: filepath.Join(homeDir, "Library", "Logs", "Framelog", "crash.log"),
		RunAtLoad:    true,
		KeepAlive:    true,
	}
	var buf bytes.Buffer
	if err := plistTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("launchd: render plist: %w", err)
	}
	return buf.Bytes(), nil
}

// Install writes the plist to plistPath and bootstraps the launchd agent.
// launchctlPath must be the output of FindLaunchctl (injectable for tests).
// Any running instance is booted out first (idempotent); the error from
// bootout is intentionally ignored. Install returns nil only once the new
// job is actually running — a daemon that silently fails to come back is
// what lets exports sit unsorted in processed/.
func Install(launchctlPath, plistPath, execPath, homeDir string) error {
	// Create plist directory.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("launchd: mkdir plist dir: %w", err)
	}

	// Create crash log directory so launchd doesn't fail to open it on first run.
	crashLogDir := filepath.Join(homeDir, "Library", "Logs", "Framelog")
	if err := os.MkdirAll(crashLogDir, 0o755); err != nil {
		return fmt.Errorf("launchd: mkdir crash log dir: %w", err)
	}

	plist, err := GeneratePlist(execPath, homeDir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("launchd: write plist: %w", err)
	}

	uid := fmt.Sprintf("%d", os.Getuid())
	domain := "gui/" + uid
	target := domain + "/" + Label

	// Bootout the old instance — ignore errors (it may not be loaded yet).
	exec.Command(launchctlPath, "bootout", target).Run() //nolint:errcheck

	// bootout returns before the old job is fully torn down. Bootstrapping
	// while the label is still registered fails ("Bootstrap failed: 5:
	// Input/output error") and leaves no daemon running, so wait for it to go.
	if !waitFor(func() bool { return !isLoaded(launchctlPath, target) }, bootoutTimeout) {
		return fmt.Errorf("launchd: %s still loaded %s after bootout", target, bootoutTimeout)
	}

	// Bootstrap the new plist, retrying in case launchd is still settling.
	var out []byte
	for attempt := 1; ; attempt++ {
		out, err = exec.Command(launchctlPath, "bootstrap", domain, plistPath).CombinedOutput()
		if err == nil {
			break
		}
		if attempt >= bootstrapAttempts {
			return fmt.Errorf("launchd: bootstrap %s: %w\n%s", plistPath, err, out)
		}
		time.Sleep(pollInterval)
	}

	// A successful bootstrap does not guarantee launchd actually spawns the
	// job (observed: loaded, RunAtLoad=true, runs = 0). kickstart without -k
	// starts it if it isn't running and is a no-op if it already is.
	if out, err := exec.Command(launchctlPath, "kickstart", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchd: kickstart %s: %w\n%s", target, err, out)
	}
	if !waitFor(func() bool { return isRunning(launchctlPath, target) }, startTimeout) {
		return fmt.Errorf("launchd: %s bootstrapped but not running after %s", target, startTimeout)
	}
	return nil
}

// Timings for Install's launchd polling. Variables so tests can shrink them.
var (
	pollInterval      = 200 * time.Millisecond
	bootoutTimeout    = 10 * time.Second
	startTimeout      = 10 * time.Second
	bootstrapAttempts = 5
)

// waitFor polls cond every pollInterval until it returns true or timeout
// elapses. Reports whether cond became true.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// isLoaded reports whether launchd knows the job: `launchctl print` exits
// non-zero ("Could not find service") once it is gone.
func isLoaded(launchctlPath, target string) bool {
	return exec.Command(launchctlPath, "print", target).Run() == nil
}

// isRunning reports whether the job is loaded and has a live process.
func isRunning(launchctlPath, target string) bool {
	out, err := exec.Command(launchctlPath, "print", target).Output()
	return err == nil && runningState.Match(out)
}

var runningState = regexp.MustCompile(`(?m)^\s*state = running\s*$`)

// Uninstall boots out the agent and removes the plist file.
// Errors from bootout are ignored — the agent may not be loaded.
func Uninstall(launchctlPath, plistPath string) error {
	uid := fmt.Sprintf("%d", os.Getuid())
	target := fmt.Sprintf("gui/%s/%s", uid, Label)
	exec.Command(launchctlPath, "bootout", target).Run() //nolint:errcheck

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("launchd: remove plist: %w", err)
	}
	return nil
}

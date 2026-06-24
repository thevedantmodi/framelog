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
	"text/template"
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
// bootout is intentionally ignored.
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
	target := fmt.Sprintf("gui/%s/%s", uid, Label)

	// Bootout the old instance — ignore errors (it may not be loaded yet).
	exec.Command(launchctlPath, "bootout", target).Run() //nolint:errcheck

	// Bootstrap the new plist. This is the one we surface to the caller.
	out, err := exec.Command(launchctlPath, "bootstrap", "gui/"+uid, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchd: bootstrap %s: %w\n%s", plistPath, err, out)
	}
	return nil
}

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

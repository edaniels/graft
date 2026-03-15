//go:build darwin

package graft

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/edaniels/graft/errors"
)

const launchdLabel = "run.graft.daemon"

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{ .Label }}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{ .BinaryPath }}</string>
		<string>daemon</string>
		<string>--replace</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{ .OutLogPath }}</string>
	<key>StandardErrorPath</key>
	<string>{{ .ErrLogPath }}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{ .Path }}</string>
	</dict>
</dict>
</plist>
`))

// LaunchdServiceManager manages the graft daemon as a macOS launchd user agent.
type LaunchdServiceManager struct {
	homeDir    string
	runCommand func(name string, args ...string) ([]byte, error)
}

// NewServiceManager returns a ServiceManager for macOS launchd.
func NewServiceManager() (ServiceManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return &LaunchdServiceManager{
		homeDir: homeDir,
		runCommand: func(name string, args ...string) ([]byte, error) {
			return exec.CommandContext(context.Background(), name, args...).CombinedOutput()
		},
	}, nil
}

func (m *LaunchdServiceManager) plistPath() string {
	return filepath.Join(m.homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

type plistData struct {
	Label      string
	BinaryPath string
	OutLogPath string
	ErrLogPath string
	Path       string
}

func (m *LaunchdServiceManager) generatePlist(binaryPath, logDir string) ([]byte, error) {
	pathEnv := "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + filepath.Join(m.homeDir, ".local", "bin")

	data := plistData{
		Label:      launchdLabel,
		BinaryPath: binaryPath,
		OutLogPath: filepath.Join(logDir, "out.log"),
		ErrLogPath: filepath.Join(logDir, "error.log"),
		Path:       pathEnv,
	}

	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, data); err != nil {
		return nil, errors.Wrap(err)
	}

	return buf.Bytes(), nil
}

func (m *LaunchdServiceManager) Install(binaryPath string) error {
	logDir := filepath.Join(m.homeDir, ".local", "state", "graft", "local", "logs")
	if err := os.MkdirAll(logDir, DirPerms); err != nil {
		return errors.Wrap(err)
	}

	plist, err := m.generatePlist(binaryPath, logDir)
	if err != nil {
		return err
	}

	plistPath := m.plistPath()

	if err := os.MkdirAll(filepath.Dir(plistPath), DirPerms); err != nil {
		return errors.Wrap(err)
	}

	// Best-effort unload before writing the new plist, in case a stale
	// registration exists from a previous install.
	m.runCommand("launchctl", "unload", plistPath) //nolint:errcheck

	if err := os.WriteFile(plistPath, plist, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	if err := m.launchctlLoad(plistPath); err != nil {
		// Clean up: unload and remove the plist so we don't leave a
		// half-installed service.
		m.runCommand("launchctl", "unload", plistPath) //nolint:errcheck
		os.Remove(plistPath)

		return err
	}

	return nil
}

func (m *LaunchdServiceManager) Uninstall() error {
	plistPath := m.plistPath()

	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed")
		}

		return errors.Wrap(err)
	}

	// Best-effort unload before removing.
	m.runCommand("launchctl", "unload", plistPath) //nolint:errcheck

	if err := os.Remove(plistPath); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (m *LaunchdServiceManager) Status() (ServiceStatus, error) {
	st := ServiceStatus{Label: launchdLabel}

	plistPath := m.plistPath()
	if _, err := os.Stat(plistPath); err == nil {
		st.Installed = true
	}

	output, err := m.runCommand("launchctl", "list", launchdLabel)
	if err != nil {
		// Not loaded — just return what we have.
		return st, nil //nolint:nilerr // launchctl error means "not loaded", not a failure
	}

	st.Loaded = true

	parsed := parseLaunchctlList(string(output))
	st.Running = parsed.Running
	st.PID = parsed.PID
	st.BinaryPath = parsed.BinaryPath

	return st, nil
}

func (m *LaunchdServiceManager) Start() error {
	plistPath := m.plistPath()

	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed; run 'graft daemon service install' first")
		}

		return errors.Wrap(err)
	}

	return m.launchctlLoad(plistPath)
}

func (m *LaunchdServiceManager) Stop() error {
	plistPath := m.plistPath()

	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed")
		}

		return errors.Wrap(err)
	}

	if _, err := m.runCommand("launchctl", "unload", plistPath); err != nil {
		return errors.WrapPrefix(err, "launchctl unload failed")
	}

	return nil
}

// launchctlLoad loads the plist using launchctl load -w. The -w flag is
// required on modern macOS where the Background Task Management system can
// mark services as disabled; without it, launchctl load silently fails
// (exit code 0 but prints "Load failed") when a service was previously
// registered.
func (m *LaunchdServiceManager) launchctlLoad(plistPath string) error {
	output, err := m.runCommand("launchctl", "load", "-w", plistPath)
	if err != nil {
		return errors.WrapPrefix(err, "launchctl load failed")
	}

	// launchctl load can return exit code 0 even when loading fails,
	// printing "Load failed: ..." to stdout instead.
	if bytes.Contains(output, []byte("Load failed")) {
		return errors.Errorf("launchctl load failed: %s", strings.TrimSpace(string(output)))
	}

	return nil
}

var (
	pidRegex     = regexp.MustCompile(`"PID"\s*=\s*(\d+)`)
	programRegex = regexp.MustCompile(`"Program"\s*=\s*"([^"]+)"`)
)

type launchctlListResult struct {
	Running    bool
	PID        int
	BinaryPath string
}

func parseLaunchctlList(output string) launchctlListResult {
	var result launchctlListResult

	if m := pidRegex.FindStringSubmatch(output); len(m) > 1 {
		pid, err := strconv.Atoi(m[1])
		if err == nil {
			result.PID = pid
			result.Running = true
		}
	}

	if m := programRegex.FindStringSubmatch(output); len(m) > 1 {
		result.BinaryPath = strings.TrimSpace(m[1])
	}

	return result
}

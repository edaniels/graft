//go:build linux

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

const systemdUnit = "graft-daemon"

var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=Graft Daemon
After=network.target

[Service]
Type=simple
ExecStart={{ .BinaryPath }} daemon --replace
Restart=on-failure
RestartSec=5
Environment=PATH=/usr/local/bin:/usr/bin:/bin:{{ .HomeLocalBin }}
StandardOutput=append:{{ .OutLogPath }}
StandardError=append:{{ .ErrLogPath }}

[Install]
WantedBy=default.target
`))

// SystemdServiceManager manages the graft daemon as a Linux systemd user service.
type SystemdServiceManager struct {
	homeDir    string
	runCommand func(name string, args ...string) ([]byte, error)
}

// NewServiceManager returns a ServiceManager for Linux systemd.
func NewServiceManager() (ServiceManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return &SystemdServiceManager{
		homeDir: homeDir,
		runCommand: func(name string, args ...string) ([]byte, error) {
			return exec.CommandContext(context.Background(), name, args...).CombinedOutput()
		},
	}, nil
}

func (m *SystemdServiceManager) unitFilePath() string {
	return filepath.Join(m.homeDir, ".config", "systemd", "user", systemdUnit+".service")
}

type unitData struct {
	BinaryPath   string
	OutLogPath   string
	ErrLogPath   string
	HomeLocalBin string
}

func (m *SystemdServiceManager) generateUnitFile(binaryPath, logDir string) ([]byte, error) {
	data := unitData{
		BinaryPath:   binaryPath,
		OutLogPath:   filepath.Join(logDir, "out.log"),
		ErrLogPath:   filepath.Join(logDir, "error.log"),
		HomeLocalBin: filepath.Join(m.homeDir, ".local", "bin"),
	}

	var buf bytes.Buffer
	if err := unitTemplate.Execute(&buf, data); err != nil {
		return nil, errors.Wrap(err)
	}

	return buf.Bytes(), nil
}

func (m *SystemdServiceManager) Install(binaryPath string) error {
	logDir := filepath.Join(m.homeDir, ".local", "state", "graft", "local", "logs")
	if err := os.MkdirAll(logDir, DirPerms); err != nil {
		return errors.Wrap(err)
	}

	unit, err := m.generateUnitFile(binaryPath, logDir)
	if err != nil {
		return err
	}

	unitPath := m.unitFilePath()

	if err := os.MkdirAll(filepath.Dir(unitPath), DirPerms); err != nil {
		return errors.Wrap(err)
	}

	if err := os.WriteFile(unitPath, unit, FilePerms); err != nil {
		return errors.Wrap(err)
	}

	if _, err := m.runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return errors.WrapPrefix(err, "systemctl daemon-reload failed")
	}

	if _, err := m.runCommand("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
		return errors.WrapPrefix(err, "systemctl enable failed")
	}

	return nil
}

func (m *SystemdServiceManager) Uninstall() error {
	unitPath := m.unitFilePath()

	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed")
		}

		return errors.Wrap(err)
	}

	if _, err := m.runCommand("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		return errors.WrapPrefix(err, "systemctl disable failed")
	}

	if err := os.Remove(unitPath); err != nil {
		return errors.Wrap(err)
	}

	if _, err := m.runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return errors.WrapPrefix(err, "systemctl daemon-reload failed")
	}

	return nil
}

func (m *SystemdServiceManager) Status() (ServiceStatus, error) {
	st := ServiceStatus{Label: systemdUnit}

	unitPath := m.unitFilePath()
	if _, err := os.Stat(unitPath); err == nil {
		st.Installed = true
	}

	output, err := m.runCommand(
		"systemctl", "--user", "show", systemdUnit,
		"--property=LoadState,ActiveState,MainPID,ExecStart", "--no-pager",
	)
	if err != nil {
		return st, nil //nolint:nilerr // systemctl error means we can't query, not a failure
	}

	parsed := parseSystemctlShow(string(output))
	st.Loaded = parsed.Loaded
	st.Running = parsed.Running
	st.PID = parsed.PID
	st.BinaryPath = parsed.BinaryPath

	return st, nil
}

func (m *SystemdServiceManager) Start() error {
	unitPath := m.unitFilePath()

	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed; run 'graft daemon service install' first")
		}

		return errors.Wrap(err)
	}

	if _, err := m.runCommand("systemctl", "--user", "start", systemdUnit); err != nil {
		return errors.WrapPrefix(err, "systemctl start failed")
	}

	return nil
}

func (m *SystemdServiceManager) Stop() error {
	unitPath := m.unitFilePath()

	if _, err := os.Stat(unitPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("service is not installed")
		}

		return errors.Wrap(err)
	}

	if _, err := m.runCommand("systemctl", "--user", "stop", systemdUnit); err != nil {
		return errors.WrapPrefix(err, "systemctl stop failed")
	}

	return nil
}

var execStartPathRegex = regexp.MustCompile(`path=([^ ;]+)`)

type systemctlShowResult struct {
	Loaded     bool
	Running    bool
	PID        int
	BinaryPath string
}

func parseSystemctlShow(output string) systemctlShowResult {
	var result systemctlShowResult

	for line := range strings.SplitSeq(output, "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := parts[0]
		value := parts[1]

		switch key {
		case "LoadState":
			result.Loaded = value == "loaded"
		case "ActiveState":
			result.Running = value == "active"
		case "MainPID":
			if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
				result.PID = pid
			}
		case "ExecStart":
			if m := execStartPathRegex.FindStringSubmatch(value); len(m) > 1 {
				result.BinaryPath = m[1]
			}
		}
	}

	return result
}

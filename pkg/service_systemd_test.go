//go:build linux

package graft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

func TestGenerateUnitFile(t *testing.T) {
	mgr := &SystemdServiceManager{
		homeDir: "/home/testuser",
	}

	unit, err := mgr.generateUnitFile("/usr/local/bin/graft", "/home/testuser/.local/state/graft/local/logs")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, unit, test.ShouldNotBeEmpty)

	unitStr := string(unit)

	// Check ExecStart
	test.That(t, strings.Contains(unitStr, "ExecStart=/usr/local/bin/graft daemon --replace"), test.ShouldBeTrue)

	// Check Restart policy
	test.That(t, strings.Contains(unitStr, "Restart=on-failure"), test.ShouldBeTrue)
	test.That(t, strings.Contains(unitStr, "RestartSec=5"), test.ShouldBeTrue)

	// Check log paths
	test.That(t, strings.Contains(unitStr, "StandardOutput=append:/home/testuser/.local/state/graft/local/logs/out.log"), test.ShouldBeTrue)
	test.That(t, strings.Contains(unitStr, "StandardError=append:/home/testuser/.local/state/graft/local/logs/error.log"), test.ShouldBeTrue)

	// Check PATH includes ~/.local/bin
	test.That(t, strings.Contains(unitStr, "/home/testuser/.local/bin"), test.ShouldBeTrue)

	// No --detach flag
	test.That(t, strings.Contains(unitStr, "detach"), test.ShouldBeFalse)

	// Check WantedBy for auto-start
	test.That(t, strings.Contains(unitStr, "WantedBy=default.target"), test.ShouldBeTrue)

	// All paths should be absolute (no ~)
	test.That(t, strings.Contains(unitStr, "~"), test.ShouldBeFalse)
}

func TestGenerateUnitFileValidINI(t *testing.T) {
	mgr := &SystemdServiceManager{
		homeDir: "/home/testuser",
	}

	unit, err := mgr.generateUnitFile("/usr/local/bin/graft", "/home/testuser/.local/state/graft/local/logs")
	test.That(t, err, test.ShouldBeNil)

	unitStr := string(unit)

	// Basic INI structure checks
	test.That(t, strings.Contains(unitStr, "[Unit]"), test.ShouldBeTrue)
	test.That(t, strings.Contains(unitStr, "[Service]"), test.ShouldBeTrue)
	test.That(t, strings.Contains(unitStr, "[Install]"), test.ShouldBeTrue)
}

func TestParseSystemctlShowRunning(t *testing.T) {
	output := `LoadState=loaded
ActiveState=active
MainPID=12345
ExecStart={ path=/usr/local/bin/graft ; argv[]=/usr/local/bin/graft daemon --replace ; }`

	result := parseSystemctlShow(output)
	test.That(t, result.Loaded, test.ShouldBeTrue)
	test.That(t, result.Running, test.ShouldBeTrue)
	test.That(t, result.PID, test.ShouldEqual, 12345)
	test.That(t, result.BinaryPath, test.ShouldEqual, "/usr/local/bin/graft")
}

func TestParseSystemctlShowNotRunning(t *testing.T) {
	output := `LoadState=loaded
ActiveState=inactive
MainPID=0
ExecStart={ path=/usr/local/bin/graft ; argv[]=/usr/local/bin/graft daemon --replace ; }`

	result := parseSystemctlShow(output)
	test.That(t, result.Loaded, test.ShouldBeTrue)
	test.That(t, result.Running, test.ShouldBeFalse)
	test.That(t, result.PID, test.ShouldEqual, 0)
}

func TestParseSystemctlShowNotLoaded(t *testing.T) {
	output := `LoadState=not-found
ActiveState=inactive
MainPID=0
ExecStart=`

	result := parseSystemctlShow(output)
	test.That(t, result.Loaded, test.ShouldBeFalse)
	test.That(t, result.Running, test.ShouldBeFalse)
	test.That(t, result.PID, test.ShouldEqual, 0)
	test.That(t, result.BinaryPath, test.ShouldBeEmpty)
}

func TestInstallWritesUnitFileAndEnables(t *testing.T) {
	tmpDir := t.TempDir()

	var commands []string

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "systemctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := mgr.Install("/usr/local/bin/graft")
	test.That(t, err, test.ShouldBeNil)

	// Verify unit file was written
	unitPath := filepath.Join(tmpDir, ".config", "systemd", "user", "graft-daemon.service")
	data, err := os.ReadFile(unitPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.Contains(string(data), "ExecStart=/usr/local/bin/graft"), test.ShouldBeTrue)

	// Verify log directory was created
	logsDir := filepath.Join(tmpDir, ".local", "state", "graft", "local", "logs")
	_, err = os.Stat(logsDir)
	test.That(t, err, test.ShouldBeNil)

	// Verify systemctl daemon-reload + enable --now were called
	test.That(t, len(commands), test.ShouldEqual, 2)
	test.That(t, strings.Contains(commands[0], "daemon-reload"), test.ShouldBeTrue)
	test.That(t, strings.Contains(commands[1], "enable --now graft-daemon"), test.ShouldBeTrue)
}

func TestUninstallDisablesAndRemoves(t *testing.T) {
	tmpDir := t.TempDir()
	unitDir := filepath.Join(tmpDir, ".config", "systemd", "user")
	test.That(t, os.MkdirAll(unitDir, 0o755), test.ShouldBeNil)

	unitPath := filepath.Join(unitDir, "graft-daemon.service")
	test.That(t, os.WriteFile(unitPath, []byte("[Unit]"), FilePerms), test.ShouldBeNil)

	var commands []string

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "systemctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := mgr.Uninstall()
	test.That(t, err, test.ShouldBeNil)

	// Verify disable --now was called, file removed, daemon-reload called
	test.That(t, len(commands), test.ShouldEqual, 2)
	test.That(t, strings.Contains(commands[0], "disable --now graft-daemon"), test.ShouldBeTrue)
	test.That(t, strings.Contains(commands[1], "daemon-reload"), test.ShouldBeTrue)

	// Verify unit file was removed
	_, err = os.Stat(unitPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestUninstallNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return nil, nil
		},
	}

	err := mgr.Uninstall()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "not installed"), test.ShouldBeTrue)
}

func TestStatusNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return []byte("LoadState=not-found\nActiveState=inactive\nMainPID=0\nExecStart=\n"), nil
		},
	}

	status, err := mgr.Status()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status.Installed, test.ShouldBeFalse)
	test.That(t, status.Loaded, test.ShouldBeFalse)
	test.That(t, status.Running, test.ShouldBeFalse)
	test.That(t, status.Label, test.ShouldEqual, systemdUnit)
}

func TestStatusInstalledButNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	unitDir := filepath.Join(tmpDir, ".config", "systemd", "user")
	test.That(t, os.MkdirAll(unitDir, 0o755), test.ShouldBeNil)

	unitPath := filepath.Join(unitDir, "graft-daemon.service")
	test.That(t, os.WriteFile(unitPath, []byte("[Unit]"), FilePerms), test.ShouldBeNil)

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return []byte("LoadState=loaded\nActiveState=inactive\nMainPID=0\nExecStart={ path=/usr/local/bin/graft ; }\n"), nil
		},
	}

	status, err := mgr.Status()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status.Installed, test.ShouldBeTrue)
	test.That(t, status.Loaded, test.ShouldBeTrue)
	test.That(t, status.Running, test.ShouldBeFalse)
}

func TestStatusRunning(t *testing.T) {
	tmpDir := t.TempDir()
	unitDir := filepath.Join(tmpDir, ".config", "systemd", "user")
	test.That(t, os.MkdirAll(unitDir, 0o755), test.ShouldBeNil)

	unitPath := filepath.Join(unitDir, "graft-daemon.service")
	test.That(t, os.WriteFile(unitPath, []byte("[Unit]"), FilePerms), test.ShouldBeNil)

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return []byte("LoadState=loaded\nActiveState=active\nMainPID=42\nExecStart={ path=/usr/local/bin/graft ; }\n"), nil
		},
	}

	status, err := mgr.Status()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status.Installed, test.ShouldBeTrue)
	test.That(t, status.Loaded, test.ShouldBeTrue)
	test.That(t, status.Running, test.ShouldBeTrue)
	test.That(t, status.PID, test.ShouldEqual, 42)
	test.That(t, status.BinaryPath, test.ShouldEqual, "/usr/local/bin/graft")
}

func testSystemctlCommand(t *testing.T, action string, run func(mgr *SystemdServiceManager) error) {
	t.Helper()

	tmpDir := t.TempDir()
	unitDir := filepath.Join(tmpDir, ".config", "systemd", "user")
	test.That(t, os.MkdirAll(unitDir, 0o755), test.ShouldBeNil)

	unitPath := filepath.Join(unitDir, "graft-daemon.service")
	test.That(t, os.WriteFile(unitPath, []byte("[Unit]"), FilePerms), test.ShouldBeNil)

	var commands []string

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "systemctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := run(mgr)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(commands), test.ShouldEqual, 1)
	test.That(t, strings.Contains(commands[0], action+" graft-daemon"), test.ShouldBeTrue)
}

func TestStartCallsSystemctlStart(t *testing.T) {
	testSystemctlCommand(t, "start", func(mgr *SystemdServiceManager) error {
		return mgr.Start()
	})
}

func TestStopCallsSystemctlStop(t *testing.T) {
	testSystemctlCommand(t, "stop", func(mgr *SystemdServiceManager) error {
		return mgr.Stop()
	})
}

func TestStartNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return nil, nil
		},
	}

	err := mgr.Start()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "not installed"), test.ShouldBeTrue)
}

func TestStopNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &SystemdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return nil, nil
		},
	}

	err := mgr.Stop()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "not installed"), test.ShouldBeTrue)
}

func TestUnitFilePathAbsolute(t *testing.T) {
	mgr := &SystemdServiceManager{
		homeDir: "/home/testuser",
	}

	path := mgr.unitFilePath()
	test.That(t, filepath.IsAbs(path), test.ShouldBeTrue)
	test.That(t, path, test.ShouldEqual, "/home/testuser/.config/systemd/user/graft-daemon.service")
}

// Suppress unused import warning.
var _ = errors.New

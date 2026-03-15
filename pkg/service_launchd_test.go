//go:build darwin

package graft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

var errLaunchctlNotLoaded = errors.New("exit status 113")

func TestGeneratePlist(t *testing.T) {
	mgr := &LaunchdServiceManager{
		homeDir: "/Users/testuser",
	}

	plist, err := mgr.generatePlist("/usr/local/bin/graft", "/Users/testuser/.local/state/graft/local/logs")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, plist, test.ShouldNotBeEmpty)

	plistStr := string(plist)

	// Check label
	test.That(t, strings.Contains(plistStr, "<string>run.graft.daemon</string>"), test.ShouldBeTrue)

	// Check binary path in ProgramArguments
	test.That(t, strings.Contains(plistStr, "<string>/usr/local/bin/graft</string>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<string>daemon</string>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<string>--replace</string>"), test.ShouldBeTrue)

	// No --detach flag
	test.That(t, strings.Contains(plistStr, "detach"), test.ShouldBeFalse)

	// Check RunAtLoad and KeepAlive
	test.That(t, strings.Contains(plistStr, "<key>RunAtLoad</key>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<key>KeepAlive</key>"), test.ShouldBeTrue)

	// Check log paths
	test.That(t, strings.Contains(plistStr, "<key>StandardOutPath</key>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<string>/Users/testuser/.local/state/graft/local/logs/out.log</string>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<key>StandardErrorPath</key>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<string>/Users/testuser/.local/state/graft/local/logs/error.log</string>"), test.ShouldBeTrue)

	// Check PATH env var includes ~/.local/bin
	test.That(t, strings.Contains(plistStr, "<key>PATH</key>"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "/Users/testuser/.local/bin"), test.ShouldBeTrue)

	// All paths should be absolute (no ~)
	test.That(t, strings.Contains(plistStr, "~"), test.ShouldBeFalse)
}

func TestGeneratePlistValidXML(t *testing.T) {
	mgr := &LaunchdServiceManager{
		homeDir: "/Users/testuser",
	}

	plist, err := mgr.generatePlist("/usr/local/bin/graft", "/Users/testuser/.local/state/graft/local/logs")
	test.That(t, err, test.ShouldBeNil)

	plistStr := string(plist)

	// Basic XML structure checks
	test.That(t, strings.HasPrefix(plistStr, "<?xml version="), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "<plist version=\"1.0\">"), test.ShouldBeTrue)
	test.That(t, strings.Contains(plistStr, "</plist>"), test.ShouldBeTrue)
}

func TestParseLaunchctlListRunning(t *testing.T) {
	// launchctl list output when the service is loaded and running
	output := `{
	"StandardOutPath" = "/Users/testuser/.local/state/graft/local/logs/out.log";
	"LimitLoadToSessionType" = "Aqua";
	"StandardErrorPath" = "/Users/testuser/.local/state/graft/local/logs/error.log";
	"Label" = "run.graft.daemon";
	"OnDemand" = false;
	"LastExitStatus" = 0;
	"PID" = 12345;
	"Program" = "/usr/local/bin/graft";
};`

	status := parseLaunchctlList(output)
	test.That(t, status.Running, test.ShouldBeTrue)
	test.That(t, status.PID, test.ShouldEqual, 12345)
}

func TestParseLaunchctlListNotRunning(t *testing.T) {
	// launchctl list output when the service is loaded but not running (no PID)
	output := `{
	"StandardOutPath" = "/Users/testuser/.local/state/graft/local/logs/out.log";
	"LimitLoadToSessionType" = "Aqua";
	"StandardErrorPath" = "/Users/testuser/.local/state/graft/local/logs/error.log";
	"Label" = "run.graft.daemon";
	"OnDemand" = false;
	"LastExitStatus" = 256;
};`

	status := parseLaunchctlList(output)
	test.That(t, status.Running, test.ShouldBeFalse)
	test.That(t, status.PID, test.ShouldEqual, 0)
}

func TestInstallWritesPlistAndLoads(t *testing.T) {
	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	logsDir := filepath.Join(tmpDir, ".local", "state", "graft", "local", "logs")

	var commands []string

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "launchctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := mgr.Install("/usr/local/bin/graft")
	test.That(t, err, test.ShouldBeNil)

	// Verify plist was written
	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	data, err := os.ReadFile(plistPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.Contains(string(data), "run.graft.daemon"), test.ShouldBeTrue)

	// Verify log directory was created
	_, err = os.Stat(logsDir)
	test.That(t, err, test.ShouldBeNil)

	// Verify best-effort unload was called before load -w
	test.That(t, len(commands), test.ShouldEqual, 2)
	test.That(t, strings.Contains(commands[0], "launchctl unload"), test.ShouldBeTrue)
	test.That(t, strings.Contains(commands[1], "launchctl load -w"), test.ShouldBeTrue)
	test.That(t, strings.Contains(commands[1], plistPath), test.ShouldBeTrue)
}

func TestInstallCleansUpOnLoadFailure(t *testing.T) {
	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := strings.Join(args, " ")
			if strings.Contains(cmd, "load -w") {
				// Simulate launchctl load returning exit 0 but printing failure.
				return []byte("Load failed: 5: Input/output error\n"), nil
			}

			return nil, nil
		},
	}

	err := mgr.Install("/usr/local/bin/graft")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "Load failed"), test.ShouldBeTrue)

	// Verify plist was cleaned up.
	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	_, statErr := os.Stat(plistPath)
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
}

func TestUninstallUnloadsAndRemoves(t *testing.T) {
	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	test.That(t, os.WriteFile(plistPath, []byte("<plist/>"), FilePerms), test.ShouldBeNil)

	var commands []string

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "launchctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := mgr.Uninstall()
	test.That(t, err, test.ShouldBeNil)

	// Verify launchctl unload was called
	test.That(t, len(commands), test.ShouldEqual, 1)
	test.That(t, strings.Contains(commands[0], "launchctl unload"), test.ShouldBeTrue)

	// Verify plist was removed
	_, err = os.Stat(plistPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestUninstallNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &LaunchdServiceManager{
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

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return nil, errLaunchctlNotLoaded
		},
	}

	status, err := mgr.Status()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status.Installed, test.ShouldBeFalse)
	test.That(t, status.Loaded, test.ShouldBeFalse)
	test.That(t, status.Running, test.ShouldBeFalse)
	test.That(t, status.Label, test.ShouldEqual, launchdLabel)
}

func TestStatusInstalledButNotLoaded(t *testing.T) {
	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	test.That(t, os.WriteFile(plistPath, []byte("<plist/>"), FilePerms), test.ShouldBeNil)

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			// launchctl list returns error when not loaded
			return nil, errLaunchctlNotLoaded
		},
	}

	status, err := mgr.Status()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status.Installed, test.ShouldBeTrue)
	test.That(t, status.Loaded, test.ShouldBeFalse)
	test.That(t, status.Running, test.ShouldBeFalse)
}

func TestStatusRunning(t *testing.T) {
	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	test.That(t, os.WriteFile(plistPath, []byte("<plist/>"), FilePerms), test.ShouldBeNil)

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return []byte(`{
	"Label" = "run.graft.daemon";
	"PID" = 42;
	"Program" = "/usr/local/bin/graft";
};`), nil
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

func testLaunchctlCommand(t *testing.T, action func(mgr *LaunchdServiceManager) error, expectedSubcommand string) {
	t.Helper()

	tmpDir := t.TempDir()
	launchAgentsDir := filepath.Join(tmpDir, "Library", "LaunchAgents")
	test.That(t, os.MkdirAll(launchAgentsDir, 0o755), test.ShouldBeNil)

	plistPath := filepath.Join(launchAgentsDir, "run.graft.daemon.plist")
	test.That(t, os.WriteFile(plistPath, []byte("<plist/>"), FilePerms), test.ShouldBeNil)

	var commands []string

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, args ...string) ([]byte, error) {
			cmd := "launchctl " + strings.Join(args, " ")
			commands = append(commands, cmd)

			return nil, nil
		},
	}

	err := action(mgr)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(commands), test.ShouldEqual, 1)
	test.That(t, strings.Contains(commands[0], "launchctl "+expectedSubcommand), test.ShouldBeTrue)
}

func TestStartCallsLaunchctlLoad(t *testing.T) {
	testLaunchctlCommand(t, (*LaunchdServiceManager).Start, "load -w")
}

func TestStopCallsLaunchctlUnload(t *testing.T) {
	testLaunchctlCommand(t, (*LaunchdServiceManager).Stop, "unload")
}

func TestLoadDetectsOutputFailure(t *testing.T) {
	mgr := &LaunchdServiceManager{
		homeDir: "/Users/testuser",
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			// launchctl load can return exit code 0 but print failure.
			return []byte("Load failed: 5: Input/output error\n"), nil
		},
	}

	err := mgr.launchctlLoad("/fake/path")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "Load failed"), test.ShouldBeTrue)
}

func TestStartNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	mgr := &LaunchdServiceManager{
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

	mgr := &LaunchdServiceManager{
		homeDir: tmpDir,
		runCommand: func(_ string, _ ...string) ([]byte, error) {
			return nil, nil
		},
	}

	err := mgr.Stop()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, strings.Contains(err.Error(), "not installed"), test.ShouldBeTrue)
}

func TestPlistPathAbsolute(t *testing.T) {
	mgr := &LaunchdServiceManager{
		homeDir: "/Users/testuser",
	}

	path := mgr.plistPath()
	test.That(t, filepath.IsAbs(path), test.ShouldBeTrue)
	test.That(t, path, test.ShouldEqual, "/Users/testuser/Library/LaunchAgents/run.graft.daemon.plist")
}

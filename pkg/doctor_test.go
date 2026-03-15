package graft

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.viam.com/test"
	"golang.org/x/crypto/ssh"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func TestDoctorCheckShellActivation(t *testing.T) {
	t.Run("active session", func(t *testing.T) {
		result := CheckShellActivation(func(key string) (string, bool) {
			if key == "GRAFT_SESSION" {
				return "12345", true
			}

			return "", false
		})
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Name, test.ShouldEqual, "Shell activation")
		test.That(t, result.Message, test.ShouldContainSubstring, "12345")
	})

	t.Run("not active", func(t *testing.T) {
		result := CheckShellActivation(func(_ string) (string, bool) {
			return "", false
		})
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "not active")
	})

	t.Run("malformed session", func(t *testing.T) {
		result := CheckShellActivation(func(key string) (string, bool) {
			if key == "GRAFT_SESSION" {
				return "notanumber", true
			}

			return "", false
		})
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "malformed")
	})
}

const testVersion = "v1.2.3"

func TestDoctorCheckLocalDaemon(t *testing.T) {
	t.Run("daemon running", func(t *testing.T) {
		ver := testVersion
		result := CheckLocalDaemon(func(_ context.Context) (*graftv1.StatusResponse, error) {
			return &graftv1.StatusResponse{
				Healthy:     true,
				VersionInfo: &graftv1.VersionInfo{Version: &ver},
			}, nil
		})
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Name, test.ShouldEqual, "Local daemon")
		test.That(t, result.Message, test.ShouldContainSubstring, "v1.2.3")
	})

	t.Run("daemon not running", func(t *testing.T) {
		result := CheckLocalDaemon(func(_ context.Context) (*graftv1.StatusResponse, error) {
			return nil, ErrDaemonNotRunning
		})
		test.That(t, result.Status, test.ShouldEqual, CheckFail)
		test.That(t, result.Message, test.ShouldContainSubstring, "not running")
	})

	t.Run("daemon error", func(t *testing.T) {
		result := CheckLocalDaemon(func(_ context.Context) (*graftv1.StatusResponse, error) {
			return nil, errors.New("connection refused")
		})
		test.That(t, result.Status, test.ShouldEqual, CheckFail)
		test.That(t, result.Message, test.ShouldContainSubstring, "connection refused")
	})
}

func TestDoctorCheckUpdates(t *testing.T) {
	t.Run("up to date", func(t *testing.T) {
		result := CheckUpdates(context.Background(), &fakeReleaseClient{latestVersion: "v1.0.0"}, "v1.0.0")
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Message, test.ShouldContainSubstring, "up to date")
	})

	t.Run("update available", func(t *testing.T) {
		result := CheckUpdates(context.Background(), &fakeReleaseClient{latestVersion: "v2.0.0"}, "v1.0.0")
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "v2.0.0")
	})

	t.Run("dev build", func(t *testing.T) {
		result := CheckUpdates(context.Background(), &fakeReleaseClient{latestVersion: "v1.0.0"}, "dev-abc1234")
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "dev build")
	})

	t.Run("check error", func(t *testing.T) {
		result := CheckUpdates(context.Background(), &fakeReleaseClient{latestErr: errors.New("network error")}, "v1.0.0")
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "network error")
	})
}

func TestDoctorResolveSSHDetails(t *testing.T) {
	t.Run("resolves config", func(t *testing.T) {
		// Create a temp file to act as an identity file.
		tmpFile := filepath.Join(t.TempDir(), "id_ed25519")
		test.That(t, os.WriteFile(tmpFile, []byte("key"), 0o600), test.ShouldBeNil)

		resolver := &fakeSSHConfigResolver{
			values: map[string]map[string]string{
				"myhost": {
					"Hostname": "actual.host.com",
					"Port":     "2222",
					"User":     "ubuntu",
				},
			},
			allValues: map[string]map[string][]string{
				"myhost": {
					"IdentityFile": {tmpFile, "/nonexistent/key"},
				},
			},
		}
		details := ResolveSSHDetails("myhost", "", "", resolver)
		detailMap := detailsToMap(details)
		test.That(t, detailMap["Hostname"], test.ShouldEqual, "actual.host.com")
		test.That(t, detailMap["Port"], test.ShouldEqual, "2222")
		test.That(t, detailMap["User"], test.ShouldEqual, "ubuntu")
		test.That(t, detailMap["Identity"], test.ShouldEqual, tmpFile)
	})

	t.Run("uses fallbacks", func(t *testing.T) {
		resolver := &fakeSSHConfigResolver{
			values:    map[string]map[string]string{},
			allValues: map[string]map[string][]string{},
		}
		details := ResolveSSHDetails("myhost", "3333", "myuser", resolver)
		detailMap := detailsToMap(details)
		test.That(t, detailMap["Hostname"], test.ShouldEqual, "myhost")
		test.That(t, detailMap["Port"], test.ShouldEqual, "3333")
		test.That(t, detailMap["User"], test.ShouldEqual, "myuser")
	})
}

func TestDoctorCheckTransportMode(t *testing.T) {
	t.Run("UDS supported via ConnectionFailed", func(t *testing.T) {
		result := CheckTransportMode(func() error {
			return &ssh.OpenChannelError{Reason: ssh.ConnectionFailed}
		})
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Message, test.ShouldContainSubstring, "UDS")
	})

	t.Run("UDS supported via successful dial", func(t *testing.T) {
		result := CheckTransportMode(func() error {
			return nil
		})
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Message, test.ShouldContainSubstring, "UDS")
	})

	t.Run("stdio fallback via Prohibited", func(t *testing.T) {
		result := CheckTransportMode(func() error {
			return &ssh.OpenChannelError{Reason: ssh.Prohibited}
		})
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "stdio")
	})

	t.Run("stdio fallback via UnknownChannelType", func(t *testing.T) {
		result := CheckTransportMode(func() error {
			return &ssh.OpenChannelError{Reason: ssh.UnknownChannelType}
		})
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "stdio")
	})

	t.Run("unknown error", func(t *testing.T) {
		result := CheckTransportMode(func() error {
			return errors.New("something weird")
		})
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "unable to determine")
	})
}

func TestDoctorCheckRemoteEnvironment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		connector := &fakeDoctorConnector{
			oneShotOutput: "Linux\namd64\n/home/ubuntu",
		}
		result, info := CheckRemoteEnvironment(context.Background(), connector)
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Name, test.ShouldEqual, "Remote environment")
		test.That(t, info.OS, test.ShouldEqual, "linux")
		test.That(t, info.Arch, test.ShouldEqual, "amd64")
		test.That(t, info.HomeDir, test.ShouldEqual, "/home/ubuntu")
		test.That(t, len(result.Details), test.ShouldBeGreaterThan, 0)
	})

	t.Run("error", func(t *testing.T) {
		connector := &fakeDoctorConnector{
			oneShotErr: errors.New("command failed"),
		}
		result, info := CheckRemoteEnvironment(context.Background(), connector)
		test.That(t, result.Status, test.ShouldEqual, CheckFail)
		test.That(t, result.Message, test.ShouldContainSubstring, "command failed")
		test.That(t, info, test.ShouldResemble, RemoteEnvironmentInfo{})
	})
}

func TestDoctorCheckRemoteDaemon(t *testing.T) {
	testInfo := RemoteEnvironmentInfo{
		OS: "linux", Arch: "amd64", HomeDir: "/home/ubuntu",
		RemoteSocketPath: "/home/ubuntu/.local/state/graft/remote/graftd.sock",
	}
	localVer := testVersion
	localVersion := &graftv1.VersionInfo{Version: &localVer}

	t.Run("binary not found", func(t *testing.T) {
		connector := &fakeDoctorConnector{
			oneShotErr: errors.New("stat: no such file"),
		}
		connectFn := func(_ context.Context, _, _ string) (*graftv1.VersionInfo, bool, error) {
			return nil, false, nil
		}
		result := CheckRemoteDaemon(context.Background(), connector, testInfo, connectFn, localVersion)
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "not installed")
	})

	t.Run("binary found but daemon not running", func(t *testing.T) {
		connector := &fakeDoctorConnector{
			oneShotOutput: "ok",
		}
		connectFn := func(_ context.Context, _, _ string) (*graftv1.VersionInfo, bool, error) {
			return nil, false, nil
		}
		result := CheckRemoteDaemon(context.Background(), connector, testInfo, connectFn, localVersion)
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "not running")
	})

	t.Run("daemon running, version matches", func(t *testing.T) {
		ver := testVersion
		connector := &fakeDoctorConnector{
			oneShotOutput: "ok",
		}
		connectFn := func(_ context.Context, _, _ string) (*graftv1.VersionInfo, bool, error) {
			return &graftv1.VersionInfo{Version: &ver}, true, nil
		}
		result := CheckRemoteDaemon(context.Background(), connector, testInfo, connectFn, localVersion)
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Message, test.ShouldContainSubstring, "v1.2.3")
	})

	t.Run("connection error", func(t *testing.T) {
		connector := &fakeDoctorConnector{
			oneShotOutput: "ok",
		}
		connectFn := func(_ context.Context, _, _ string) (*graftv1.VersionInfo, bool, error) {
			return nil, false, errors.New("dial failed")
		}
		result := CheckRemoteDaemon(context.Background(), connector, testInfo, connectFn, localVersion)
		test.That(t, result.Status, test.ShouldEqual, CheckWarn)
		test.That(t, result.Message, test.ShouldContainSubstring, "not running")
	})
}

func TestDoctorCheckRemoteDirectories(t *testing.T) {
	t.Run("no identity", func(t *testing.T) {
		info := RemoteEnvironmentInfo{
			OS:               "linux",
			Arch:             "amd64",
			HomeDir:          "/home/ubuntu",
			RemoteSocketPath: "/home/ubuntu/.local/state/graft/remote/graftd.sock",
		}
		result := CheckRemoteDirectories(info)
		test.That(t, result.Status, test.ShouldEqual, CheckPass)
		test.That(t, result.Name, test.ShouldEqual, "Remote directories")

		detailMap := detailsToMap(result.Details)
		test.That(t, detailMap["Logs"], test.ShouldContainSubstring, "/remote/logs")
		test.That(t, detailMap["Binary"], test.ShouldEqual, "/home/ubuntu/graft-linux-amd64")
	})

	t.Run("with identity", func(t *testing.T) {
		info := RemoteEnvironmentInfo{
			OS:               "linux",
			Arch:             "amd64",
			HomeDir:          "/home/ubuntu",
			RemoteSocketPath: "/home/ubuntu/.local/state/graft/remote/bright-falcon-soar/graftd.sock",
		}
		result := CheckRemoteDirectories(info)
		test.That(t, result.Status, test.ShouldEqual, CheckPass)

		detailMap := detailsToMap(result.Details)
		test.That(t, detailMap["Logs"], test.ShouldContainSubstring, "bright-falcon-soar/logs")
		test.That(t, detailMap["Binary"], test.ShouldContainSubstring, "bright-falcon-soar/graft-linux-amd64")
		test.That(t, detailMap["Socket"], test.ShouldContainSubstring, "bright-falcon-soar/graftd.sock")
	})
}

func detailsToMap(details []string) map[string]string {
	m := make(map[string]string, len(details))

	for _, d := range details {
		parts := strings.SplitN(d, ":", 2)
		if len(parts) == 2 {
			m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	return m
}

func TestDoctorCheckResultFields(t *testing.T) {
	r := CheckResult{
		Name:    "Test check",
		Status:  CheckPass,
		Message: "all good",
		Details: []string{"detail one", "detail two"},
	}
	test.That(t, r.Name, test.ShouldEqual, "Test check")
	test.That(t, r.Status, test.ShouldEqual, CheckPass)
	test.That(t, r.Message, test.ShouldEqual, "all good")
	test.That(t, len(r.Details), test.ShouldEqual, 2)
}

// --- Fakes ---

type fakeDoctorConnector struct {
	oneShotOutput string
	oneShotErr    error
}

func (f *fakeDoctorConnector) RunOneShotCommand(_ context.Context, _ string) (string, error) {
	return f.oneShotOutput, f.oneShotErr
}

func (f *fakeDoctorConnector) Identity() string {
	return ""
}

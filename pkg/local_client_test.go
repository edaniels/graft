package graft

import (
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func TestConnectAndCheckSocketMissing(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	conn, svc, ver, err := connectAndCheck(t.Context(), sockPath)
	test.That(t, errors.Is(err, ErrDaemonNotRunning), test.ShouldBeTrue)
	test.That(t, conn, test.ShouldBeNil)
	test.That(t, svc, test.ShouldBeNil)
	test.That(t, ver, test.ShouldBeNil)
}

func TestConnectAndCheckStaleSocket(t *testing.T) {
	// Create a regular file at the socket path to simulate a stale socket
	// (file exists but no daemon is listening).
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "stale.sock")

	f, err := os.Create(sockPath)
	test.That(t, err, test.ShouldBeNil)
	f.Close()

	conn, svc, ver, err := connectAndCheck(t.Context(), sockPath)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, conn, test.ShouldBeNil)
	test.That(t, svc, test.ShouldBeNil)
	test.That(t, ver, test.ShouldBeNil)
}

func TestNewLocalClientDaemonNotRunning(t *testing.T) {
	// Use a temp dir for state so we don't interfere with real daemon.
	t.Setenv("GRAFT_STATE_HOME", t.TempDir())

	_, _, err := NewLocalClient(t.Context(), os.Stdout, os.Stderr, nil, false, nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, errors.Is(err, ErrDaemonNotRunning), test.ShouldBeTrue)
}

func TestVersionMismatchDetection(t *testing.T) {
	tests := []struct {
		name          string
		clientVersion *graftv1.VersionInfo
		daemonVersion *graftv1.VersionInfo
		shouldRestart bool
	}{
		{
			"same tagged version",
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			false,
		},
		{
			"different tagged versions",
			&graftv1.VersionInfo{Version: new("v1.0.0")},
			&graftv1.VersionInfo{Version: new("v2.0.0")},
			true,
		},
		{
			"same dev revision",
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("abc1234")},
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("abc1234")},
			false,
		},
		{
			"different dev revisions",
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("abc1234")},
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("def5678")},
			true,
		},
		{
			"same dev revision one dirty",
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("abc1234"), VcsModified: new(true)},
			&graftv1.VersionInfo{Version: new("(devel)"), VcsRevision: new("abc1234"), VcsModified: new(false)},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientVer := versionString(tc.clientVersion)
			daemonVer := versionString(tc.daemonVersion)

			mismatch := clientVer != daemonVer
			test.That(t, mismatch, test.ShouldEqual, tc.shouldRestart)
		})
	}
}

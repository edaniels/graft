package graft

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc"

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

type minimalStatusServer struct {
	graftv1.UnimplementedGraftServiceServer
}

func (s *minimalStatusServer) Status(
	_ context.Context, _ *graftv1.StatusRequest,
) (*graftv1.StatusResponse, error) {
	return &graftv1.StatusResponse{Healthy: true}, nil
}

func TestWaitForDaemonTimeout(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := WaitForDaemon(ctx, sockPath)
	test.That(t, err, test.ShouldNotBeNil)
}

func testSocketPath(t *testing.T, name string) string {
	t.Helper()
	// macOS limits unix socket paths to 104 characters, so we cannot use
	// t.TempDir() (whose paths are too long). Use /tmp directly instead.
	dir, err := os.MkdirTemp("/tmp", "graft-test-") //nolint:usetesting
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { os.RemoveAll(dir) })

	return filepath.Join(dir, name)
}

func TestWaitForDaemonAlreadyRunning(t *testing.T) {
	sockPath := testSocketPath(t, "test.sock")

	var lc net.ListenConfig

	lis, err := lc.Listen(t.Context(), "unix", sockPath)
	test.That(t, err, test.ShouldBeNil)

	defer lis.Close()

	grpcServer := grpc.NewServer()
	graftv1.RegisterGraftServiceServer(grpcServer, &minimalStatusServer{})

	go grpcServer.Serve(lis) //nolint:errcheck
	defer grpcServer.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err = WaitForDaemon(ctx, sockPath)
	test.That(t, err, test.ShouldBeNil)
}

func TestWaitForDaemonDelayedStart(t *testing.T) {
	sockPath := testSocketPath(t, "test.sock")

	ready := make(chan struct{})

	go func() {
		var lc net.ListenConfig

		lis, lisErr := lc.Listen(t.Context(), "unix", sockPath)
		if lisErr != nil {
			return
		}
		defer lis.Close()

		grpcServer := grpc.NewServer()
		graftv1.RegisterGraftServiceServer(grpcServer, &minimalStatusServer{})

		go grpcServer.Serve(lis) //nolint:errcheck
		defer grpcServer.Stop()

		close(ready)
		// Keep alive until test context is done.
		<-t.Context().Done()
	}()

	// Wait until server is ready before checking.
	<-ready

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := WaitForDaemon(ctx, sockPath)
	test.That(t, err, test.ShouldBeNil)
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

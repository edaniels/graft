package graft

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"go.viam.com/test"
)

// localSockDirForTest returns the socket directory NewServer will use for a
// local daemon under the test's GRAFT_STATE_HOME.
func localSockDirForTest(t *testing.T) string {
	t.Helper()

	homeDir, err := os.UserHomeDir()
	test.That(t, err, test.ShouldBeNil)

	sockPath, err := daemonSocketPath(graftStateHome(homeDir), ServerRoleLocal, "")
	test.That(t, err, test.ShouldBeNil)

	return filepath.Dir(sockPath)
}

func TestNewServerReplaceKillsExistingDaemon(t *testing.T) {
	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	// A live process standing in for the old daemon.
	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	test.That(t, cmd.Start(), test.ShouldBeNil)

	pid := cmd.Process.Pid

	waitDone := make(chan struct{})

	go func() {
		cmd.Wait() //nolint:errcheck
		close(waitDone)
	}()

	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck // best effort cleanup
		<-waitDone
	})

	sockDir := localSockDirForTest(t)
	test.That(t, os.MkdirAll(sockDir, DirPerms), test.ShouldBeNil)

	// The old daemon's socket and PID file exist.
	sockPath := filepath.Join(sockDir, "graftd.sock")
	test.That(t, os.WriteFile(sockPath, []byte{}, 0o600), test.ShouldBeNil)

	pidPath := filepath.Join(sockDir, "graftd.pid")
	test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600), test.ShouldBeNil)

	srv, err := NewServer(&RootConfig{}, ServerRoleLocal, "", true, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(srv.synchronizationManager.Shutdown)

	// The stand-in daemon was killed during startup, and its socket and PID
	// files are gone so the new daemon can bind cleanly.
	<-waitDone
	test.That(t, syscall.Kill(pid, 0), test.ShouldNotBeNil)

	_, statErr := os.Stat(pidPath)
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)

	_, statErr = os.Stat(sockPath)
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
}

func TestNewServerReplaceFailsWhenOldDaemonUnkillable(t *testing.T) {
	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	sockDir := localSockDirForTest(t)
	test.That(t, os.MkdirAll(sockDir, DirPerms), test.ShouldBeNil)

	// A PID "file" that is a directory cannot be read, so the old daemon can
	// neither be identified nor killed. Replacement must fail clearly here
	// rather than proceed to load sessions and then die confusingly on the
	// socket bind while the old daemon lives on.
	pidPath := filepath.Join(sockDir, "graftd.pid")
	test.That(t, os.Mkdir(pidPath, DirPerms), test.ShouldBeNil)

	_, err := NewServer(&RootConfig{}, ServerRoleLocal, "", true, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "replacing existing daemon")
}

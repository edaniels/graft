package graft

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"go.viam.com/test"
)

// TestSyncPersistenceE2E_DaemonRestart: after a daemon restart, a file deleted
// locally during the outage stays deleted and the deletion propagates to the
// remote.
func TestSyncPersistenceE2E_DaemonRestart(t *testing.T) {
	requireDocker(t)

	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	localDir := mkShortTempDir(t, "src-")
	test.That(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep"), 0o600), test.ShouldBeNil)
	test.That(t, os.WriteFile(filepath.Join(localDir, "delete-me.txt"), []byte("delete"), 0o600), test.ShouldBeNil)

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/syncpersist"

	connName := sanitizeContainerName("graft-e2e-syncpersist-restart-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: localDir, ToRemote: remoteDir},
			},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)

		listed := dockerLs(t, sc.containerID, remoteDir)
		test.That(t, listed, test.ShouldContainSubstring, "keep.txt")
		test.That(t, listed, test.ShouldContainSubstring, "delete-me.txt")
	})

	test.That(t, os.Remove(filepath.Join(localDir, "delete-me.txt")), test.ShouldBeNil)

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)
		waitForRemotePath(t, sc.containerID, remoteDir, "delete-me.txt", false, 15*time.Second)

		listed := dockerLs(t, sc.containerID, remoteDir)
		test.That(t, listed, test.ShouldContainSubstring, "keep.txt")
		test.That(t, listed, test.ShouldNotContainSubstring, "delete-me.txt")

		_, statErr := os.Stat(filepath.Join(localDir, "delete-me.txt"))
		test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
	})
}

// TestSyncPersistenceE2E_NewRemoteFilePropagates: a file genuinely created on
// the remote during an outage comes down on reconnect. Counterpart to the
// deletion test so we don't suppress legitimate remote-to-local propagation.
func TestSyncPersistenceE2E_NewRemoteFilePropagates(t *testing.T) {
	requireDocker(t)
	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	localDir := mkShortTempDir(t, "src-")
	test.That(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep"), 0o600), test.ShouldBeNil)

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/syncpersist"

	connName := sanitizeContainerName("graft-e2e-syncpersist-newremote-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: localDir, ToRemote: remoteDir},
			},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)
		listed := dockerLs(t, sc.containerID, remoteDir)
		test.That(t, listed, test.ShouldContainSubstring, "keep.txt")
	})

	// Create a file on the remote while the daemon is down. The ancestor
	// doesn't have it, so the post-resume scan should pull it down to local.
	dockerExec(t, sc.containerID, "echo 'new from remote' > "+remoteDir+"/from-remote.txt")

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)
		waitForLocalPath(t, filepath.Join(localDir, "from-remote.txt"), true, 15*time.Second)

		contents, readErr := os.ReadFile(filepath.Join(localDir, "from-remote.txt"))
		test.That(t, readErr, test.ShouldBeNil)
		test.That(t, string(contents), test.ShouldContainSubstring, "new from remote")
	})
}

// runSyncDaemon spins up a local graft Server, runs it, hands it to fn, and
// tears it down on the way out.
func runSyncDaemon(t *testing.T, env *e2eDockerEnv, config *RootConfig, fn func(t *testing.T, srv *Server)) {
	t.Helper()

	srv, err := NewServer(config, ServerRoleLocal, "", true, &BufferedLineWriter{MaxLines: 100}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)

	srv.connMgr.RegisterConnectorFactory(sshSchemeName, env.sshConnectorFactory(t))

	runCtx, runCancel := context.WithCancel(context.Background())

	test.That(t, srv.Run(runCtx), test.ShouldBeNil)

	// Cancel runCtx before Close so the connMgr/sessMgr/reconcile goroutines
	// exit before Close Waits on them.
	defer func() {
		runCancel()
		srv.Close()
	}()

	fn(t, srv)
}

// waitForSyncWatching polls until at least one session is Watching with no
// conflicts. Watching is the steady state at the end of a sync cycle, so
// it's safe to assert filesystem state after.
func waitForSyncWatching(t *testing.T, srv *Server) {
	t.Helper()

	const timeout = 20 * time.Second

	deadline := time.Now().Add(timeout)
	mgr := srv.synchronizationManager

	var lastErr string

	for time.Now().Before(deadline) {
		_, states, listErr := mgr.List(context.Background(), &selection.Selection{All: true}, 0)
		if listErr != nil {
			lastErr = listErr.Error()
		} else {
			for _, state := range states {
				if state.GetStatus() == synchronization.Status_Watching && len(state.GetConflicts()) == 0 {
					return
				}

				if state.GetLastError() != "" {
					lastErr = state.GetLastError()
				}
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("sync did not reach Status_Watching within %s; last error: %s", timeout, lastErr)
}

// waitForRemotePath polls for basename's presence (or absence) inside dir on
// the remote container. The first Watching after restart doesn't mean our
// delete has propagated yet; the follow-up cycle that handles it is what we're
// waiting on.
func waitForRemotePath(t *testing.T, containerID, dir, basename string, wantPresent bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		entries := strings.Split(strings.TrimSpace(dockerLs(t, containerID, dir)), "\n")

		if slices.Contains(entries, basename) == wantPresent {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("remote path %s/%s present=%v not satisfied within %s", dir, basename, wantPresent, timeout)
}

// waitForLocalPath is the local-side counterpart to waitForRemotePath.
func waitForLocalPath(t *testing.T, path string, wantPresent bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		present := err == nil

		if present == wantPresent {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("local path %s present=%v not satisfied within %s", path, wantPresent, timeout)
}

func dockerLs(t *testing.T, containerID, dir string) string {
	t.Helper()

	out, err := exec.Command("docker", "exec", containerID, "ls", "-A", dir).CombinedOutput() //nolint:noctx // test helper
	test.That(t, err, test.ShouldBeNil)

	return string(out)
}

// mkShortTempDir returns a temp dir under /tmp with a short prefix. macOS caps
// unix socket paths at 104 bytes, so t.TempDir's long prefix breaks the bind.
func mkShortTempDir(t *testing.T, prefix string) string {
	t.Helper()

	d, err := os.MkdirTemp("/tmp", prefix) //nolint:usetesting // need short path for unix sockets on macOS
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() {
		os.RemoveAll(d)
	})

	return d
}

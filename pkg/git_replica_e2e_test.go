package graft

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"go.viam.com/test"
)

// TestGitReplicaSyncE2E: a synchronization with SyncGit replicates the local
// .git to the remote one-way; remote-side writes into .git are reverted,
// transient lock files never propagate, and disabling the flag reaps the
// replica session while leaving the remote .git contents in place.
func TestGitReplicaSyncE2E(t *testing.T) {
	requireDocker(t)

	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	localDir := mkShortTempDir(t, "src-")
	test.That(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep"), 0o600), test.ShouldBeNil)

	gitInitOut, gitInitErr := exec.Command("git", "init", localDir).CombinedOutput() //nolint:noctx // test helper
	test.That(t, gitInitErr, test.ShouldBeNil)
	test.That(t, gitInitOut, test.ShouldNotBeNil)

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/gitreplica"

	connName := sanitizeContainerName("graft-e2e-gitreplica-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: localDir, ToRemote: remoteDir, SyncGit: true},
			},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)

		// Both the working tree and the .git replica arrive.
		waitForRemotePath(t, sc.containerID, remoteDir, "keep.txt", true, 30*time.Second)
		waitForRemotePath(t, sc.containerID, remoteDir+"/.git", "HEAD", true, 30*time.Second)

		// One-way replica: a remote-side write into .git is reverted.
		dockerExec(t, sc.containerID, "touch "+remoteDir+"/.git/remote-junk")
		waitForRemotePath(t, sc.containerID, remoteDir+"/.git", "remote-junk", false, 30*time.Second)

		// Transient lock files never propagate. The marker file that follows
		// proves a replica cycle ran after the lock was created.
		test.That(t, os.WriteFile(filepath.Join(localDir, ".git", "index.lock"), []byte("lock"), 0o600), test.ShouldBeNil)
		test.That(t, os.WriteFile(filepath.Join(localDir, ".git", "marker.txt"), []byte("marker"), 0o600), test.ShouldBeNil)
		waitForRemotePath(t, sc.containerID, remoteDir+"/.git", "marker.txt", true, 30*time.Second)

		listed := dockerLs(t, sc.containerID, remoteDir+"/.git")
		test.That(t, listed, test.ShouldNotContainSubstring, "index.lock")

		test.That(t, countGraftSessions(t, srv), test.ShouldEqual, 2)
	})

	// Disable the flag: on the next daemon run the replica session is
	// reaped, but the already-replicated remote .git contents stay on disk.
	config.Connections[0].Synchronizations[0].SyncGit = false

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)
		waitForGraftSessionCount(t, srv, 1, 60*time.Second)

		listed := dockerLs(t, sc.containerID, remoteDir+"/.git")
		test.That(t, listed, test.ShouldContainSubstring, "HEAD")
	})
}

// countGraftSessions returns how many graft-owned sync sessions the manager
// currently holds.
func countGraftSessions(t *testing.T, srv *Server) int {
	t.Helper()

	_, states, err := srv.synchronizationManager.List(context.Background(), &selection.Selection{All: true}, 0)
	test.That(t, err, test.ShouldBeNil)

	var count int

	for _, state := range states {
		if isGraftSyncName(state.GetSession().GetName()) {
			count++
		}
	}

	return count
}

// waitForGraftSessionCount polls until the manager holds exactly want
// graft-owned sessions; the orphan reaper runs on the reconcile loop, so a
// terminated session disappears within its interval.
func waitForGraftSessionCount(t *testing.T, srv *Server, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var last int

	for time.Now().Before(deadline) {
		last = countGraftSessions(t, srv)
		if last == want {
			return
		}

		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("graft session count %d != %d within %s", last, want, timeout)
}

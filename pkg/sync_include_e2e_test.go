package graft

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/test"
)

// TestSyncIncludeE2E_GitignoredFileSyncs is the motivating scenario: a file
// the .gitignore excludes (a generated protobuf) is generated on the remote
// (beta) and must still propagate to the local (alpha) because a syncInclude
// override re-includes it. A different gitignored file with no override stays
// remote-only, proving the base .gitignore is still honored for sync.
func TestSyncIncludeE2E_GitignoredFileSyncs(t *testing.T) {
	requireDocker(t)

	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	localDir := mkShortTempDir(t, "src-")
	test.That(t, os.WriteFile(filepath.Join(localDir, "keep.txt"), []byte("keep"), 0o600), test.ShouldBeNil)
	// .gitignore excludes generated protobufs and logs from git (and, by
	// default, from sync). syncInclude will re-include the protobufs only.
	test.That(t, os.WriteFile(filepath.Join(localDir, ".gitignore"),
		[]byte("*_pb2.py\n*.log\n"), 0o600), test.ShouldBeNil)

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/syncinclude"

	connName := sanitizeContainerName("graft-e2e-syncinclude-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: localDir, ToRemote: remoteDir, SyncInclude: []string{"**/*_pb2.py"}},
			},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)
		test.That(t, dockerLs(t, sc.containerID, remoteDir), test.ShouldContainSubstring, "keep.txt")
	})

	// Simulate the remote running codegen while the daemon is down: it emits a
	// gitignored generated protobuf (covered by syncInclude) and a gitignored
	// log (not covered).
	dockerExec(t, sc.containerID, "echo 'generated' > "+remoteDir+"/service_pb2.py")
	dockerExec(t, sc.containerID, "echo 'noise' > "+remoteDir+"/debug.log")

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)

		// The override pulls the generated protobuf down to local.
		waitForLocalPath(t, filepath.Join(localDir, "service_pb2.py"), true, 15*time.Second)

		contents, readErr := os.ReadFile(filepath.Join(localDir, "service_pb2.py"))
		test.That(t, readErr, test.ShouldBeNil)
		test.That(t, string(contents), test.ShouldContainSubstring, "generated")

		// The un-overridden gitignored log is still excluded from sync: once the
		// protobuf (created in the same batch) has arrived, a full cycle has run,
		// so its continued absence is meaningful rather than a race.
		_, statErr := os.Stat(filepath.Join(localDir, "debug.log"))
		test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
	})
}

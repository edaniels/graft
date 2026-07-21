package graft

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.viam.com/test"
)

// TestSyncPermissionsE2E: working-tree files synced to the remote must be
// world-readable (0644, or 0755 when source-executable) and directories
// world-traversable (0755) rather than mutagen's portable defaults of
// 0600/0700, which break remote consumers running as other users (e.g.
// non-root container users reading docker bind mounts). The loosening is
// remote-only: files the sync creates locally keep mutagen's private 0600,
// and the .git replica keeps 0600/0700 on the remote. Per-sync config modes
// override the remote defaults.
func TestSyncPermissionsE2E(t *testing.T) {
	requireDocker(t)

	env := getOrSetupE2EEnv(t)

	stateDir := mkShortTempDir(t, "ste-")
	t.Setenv("GRAFT_STATE_HOME", stateDir)

	// The source modes below are the test subject. //nolint:gosec on the
	// world-readable ones: that readability is exactly what's under test.
	localDir := mkShortTempDir(t, "src-")
	test.That(t, os.WriteFile(filepath.Join(localDir, "readable.txt"), []byte("data"), 0o644), test.ShouldBeNil)     //nolint:gosec
	test.That(t, os.WriteFile(filepath.Join(localDir, "script.sh"), []byte("#!/bin/sh\n"), 0o755), test.ShouldBeNil) //nolint:gosec
	test.That(t, os.WriteFile(filepath.Join(localDir, "private.txt"), []byte("secret"), 0o600), test.ShouldBeNil)
	test.That(t, os.Mkdir(filepath.Join(localDir, "sub"), 0o755), test.ShouldBeNil)
	test.That(t, os.WriteFile(filepath.Join(localDir, "sub", "nested.txt"), []byte("nested"), 0o644), test.ShouldBeNil) //nolint:gosec

	gitInitOut, gitInitErr := exec.Command("git", "init", localDir).CombinedOutput() //nolint:noctx // test helper
	test.That(t, gitInitErr, test.ShouldBeNil)
	test.That(t, gitInitOut, test.ShouldNotBeNil)

	// A second tree exercises per-sync mode overrides.
	tightDir := mkShortTempDir(t, "tgt-")
	test.That(t, os.WriteFile(filepath.Join(tightDir, "tight.txt"), []byte("tight"), 0o644), test.ShouldBeNil) //nolint:gosec
	test.That(t, os.Mkdir(filepath.Join(tightDir, "sub"), 0o755), test.ShouldBeNil)
	test.That(t, os.WriteFile(filepath.Join(tightDir, "sub", "nested.txt"), []byte("nested"), 0o644), test.ShouldBeNil) //nolint:gosec

	sc := env.startSSHContainerInfo(t)
	remoteDir := "/home/" + e2eContainerUser + "/syncperms"
	remoteTightDir := "/home/" + e2eContainerUser + "/syncpermstight"

	connName := sanitizeContainerName("graft-e2e-syncperms-" + t.Name())
	destination := fmt.Sprintf("ssh://%s@127.0.0.1:%s", e2eContainerUser, sc.port)

	config := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        connName,
			Destination: destination,
			LocalRoot:   localDir,
			RemoteRoot:  remoteDir,
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: localDir, ToRemote: remoteDir, SyncGit: true},
				{
					FromLocal:            tightDir,
					ToRemote:             remoteTightDir,
					DefaultFileMode:      "640",
					DefaultDirectoryMode: "750",
				},
			},
		}},
	}

	runSyncDaemon(t, env, config, func(t *testing.T, srv *Server) {
		t.Helper()

		waitForSyncWatching(t, srv)

		waitForRemotePath(t, sc.containerID, remoteDir, "readable.txt", true, 30*time.Second)
		waitForRemotePath(t, sc.containerID, remoteDir+"/sub", "nested.txt", true, 30*time.Second)
		waitForRemotePath(t, sc.containerID, remoteDir+"/.git", "HEAD", true, 30*time.Second)
		waitForRemotePath(t, sc.containerID, remoteTightDir+"/sub", "nested.txt", true, 30*time.Second)

		// Working tree: world-readable remote defaults. The sync root itself
		// is created by graft (not mutagen), so its mode must be pinned
		// rather than inherited from the remote umask: a 0700 root would
		// block traversal into an otherwise world-readable tree.
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir), test.ShouldEqual, "755")
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/readable.txt"), test.ShouldEqual, "644")
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/script.sh"), test.ShouldEqual, "755")
		// Mutagen's portable permissions model can't mirror per-file source
		// modes; locally-private files land at the tree default like the rest.
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/private.txt"), test.ShouldEqual, "644")
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/sub"), test.ShouldEqual, "755")
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/sub/nested.txt"), test.ShouldEqual, "644")

		// The .git replica stays private: its contents (embedded credentials
		// in config, full history in objects) keep mutagen's 0600/0700.
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/.git"), test.ShouldEqual, "700")
		test.That(t, dockerStatMode(t, sc.containerID, remoteDir+"/.git/HEAD"), test.ShouldEqual, "600")

		// Per-sync config modes override the remote defaults, including on
		// the graft-created sync root.
		test.That(t, dockerStatMode(t, sc.containerID, remoteTightDir), test.ShouldEqual, "750")
		test.That(t, dockerStatMode(t, sc.containerID, remoteTightDir+"/tight.txt"), test.ShouldEqual, "640")
		test.That(t, dockerStatMode(t, sc.containerID, remoteTightDir+"/sub"), test.ShouldEqual, "750")

		// The loosening is remote-only: a file created on the remote comes
		// down to the local tree with mutagen's private 0600 default.
		dockerExec(t, sc.containerID, "echo fromremote > "+remoteDir+"/remote-born.txt")
		waitForLocalPath(t, filepath.Join(localDir, "remote-born.txt"), true, 30*time.Second)

		info, statErr := os.Stat(filepath.Join(localDir, "remote-born.txt"))
		test.That(t, statErr, test.ShouldBeNil)
		test.That(t, info.Mode().Perm(), test.ShouldEqual, os.FileMode(0o600))
	})
}

// dockerStatMode returns a path's octal permission bits inside a container.
func dockerStatMode(t *testing.T, containerID, path string) string {
	t.Helper()

	out, err := exec.Command( //nolint:noctx // test helper
		"docker", "exec", containerID, "stat", "-c", "%a", path,
	).CombinedOutput()
	test.That(t, err, test.ShouldBeNil)

	return strings.TrimSpace(string(out))
}

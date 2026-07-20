package graft

import (
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

func TestGitReplicaIntent(t *testing.T) {
	parent := SynchronizationIntent{FromLocal: "/home/u/src", ToRemote: "/remote/src", SyncGit: true}
	replica := gitReplicaIntent(parent)

	t.Run("derives .git endpoints", func(t *testing.T) {
		test.That(t, replica.FromLocal, test.ShouldEqual, "/home/u/src/.git")
		test.That(t, replica.ToRemote, test.ShouldEqual, "/remote/src/.git")
	})

	t.Run("session name differs from parent and is graft-owned", func(t *testing.T) {
		parentName := syncSessionName("conn", parent)
		replicaName := syncSessionName("conn", replica)

		test.That(t, replicaName, test.ShouldNotEqual, parentName)
		test.That(t, isGraftSyncName(replicaName), test.ShouldBeTrue)
	})

	t.Run("session name is deterministic", func(t *testing.T) {
		test.That(t,
			syncSessionName("conn", gitReplicaIntent(parent)),
			test.ShouldEqual,
			syncSessionName("conn", replica),
		)
	})
}

func TestSyncGitConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	conf := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        "conn",
			Destination: "ssh://u@h:22",
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: "/a", ToRemote: "/A", SyncGit: true},
				{FromLocal: "/b", ToRemote: "/B"},
			},
		}},
	}
	test.That(t, conf.Persist(path), test.ShouldBeNil)

	t.Run("false flag is omitted from yaml", func(t *testing.T) {
		data, err := os.ReadFile(path)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldContainSubstring, "syncGit: true")
	})

	t.Run("flag survives reload", func(t *testing.T) {
		reloaded := &RootConfig{}
		test.That(t, reloaded.Reload(path), test.ShouldBeNil)

		syncs := reloaded.Connections[0].Synchronizations
		test.That(t, syncs[0].SyncGit, test.ShouldBeTrue)
		test.That(t, syncs[1].SyncGit, test.ShouldBeFalse)
	})
}

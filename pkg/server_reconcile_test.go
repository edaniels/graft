package graft

import (
	"testing"

	"go.viam.com/test"
)

func TestComputeMissingSyncs(t *testing.T) {
	t.Run("nothing desired returns nothing", func(t *testing.T) {
		got := computeMissingSyncs(nil, nil)
		test.That(t, got, test.ShouldBeEmpty)
	})

	t.Run("all desired and none active returns all", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
			{FromLocal: "/b", ToRemote: "/B"},
		}

		got := computeMissingSyncs(desired, nil)
		test.That(t, len(got), test.ShouldEqual, 2)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/a")
		test.That(t, got[0].ToRemote, test.ShouldEqual, "/A")
		test.That(t, got[1].FromLocal, test.ShouldEqual, "/b")
		test.That(t, got[1].ToRemote, test.ShouldEqual, "/B")
	})

	t.Run("active exact match is skipped", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
			{FromLocal: "/b", ToRemote: "/B"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/b")
	})

	t.Run("active with different destination is treated as missing", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/different"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].FromLocal, test.ShouldEqual, "/a")
		test.That(t, got[0].ToRemote, test.ShouldEqual, "/A")
	})

	t.Run("active with different SyncGit flag is treated as missing", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", SyncGit: true},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].SyncGit, test.ShouldBeTrue)
	})
}

func TestExpectedSyncSessionNames(t *testing.T) {
	intent := SynchronizationIntentConfig{FromLocal: "/a", ToRemote: "/A"}
	gitIntent := SynchronizationIntentConfig{FromLocal: "/b", ToRemote: "/B", SyncGit: true}

	pending := []ConnectionConfig{{
		Name:             "conn",
		Synchronizations: []SynchronizationIntentConfig{intent, gitIntent},
	}}

	expected := expectedSyncSessionNames(pending)

	t.Run("every synchronization contributes its session name", func(t *testing.T) {
		test.That(t, expected[syncSessionName("conn", SynchronizationIntentFromConfig(intent))], test.ShouldBeTrue)
		test.That(t, expected[syncSessionName("conn", SynchronizationIntentFromConfig(gitIntent))], test.ShouldBeTrue)
	})

	t.Run("git replica name included only when SyncGit is set", func(t *testing.T) {
		gitName := syncSessionName("conn", gitReplicaIntent(SynchronizationIntentFromConfig(gitIntent)))
		nonGitName := syncSessionName("conn", gitReplicaIntent(SynchronizationIntentFromConfig(intent)))

		test.That(t, expected[gitName], test.ShouldBeTrue)
		test.That(t, expected[nonGitName], test.ShouldBeFalse)
		test.That(t, len(expected), test.ShouldEqual, 3)
	})
}

func TestComputeMissingForwardCommands(t *testing.T) {
	t.Run("nothing desired returns nothing", func(t *testing.T) {
		got := computeMissingForwardCommands(nil, nil)
		test.That(t, got, test.ShouldBeEmpty)
	})

	t.Run("all desired returns all when none active", func(t *testing.T) {
		desired := []ForwardCommandIntent{
			{Name: "go", Prefix: false},
			{Name: "python", Prefix: true},
		}

		got := computeMissingForwardCommands(desired, nil)
		test.That(t, len(got), test.ShouldEqual, 2)
	})

	t.Run("exact match in active is skipped", func(t *testing.T) {
		desired := []ForwardCommandIntent{
			{Name: "go", Prefix: false},
			{Name: "python", Prefix: false},
		}
		active := []ForwardCommandIntent{{Name: "go", Prefix: false}}

		got := computeMissingForwardCommands(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].Name, test.ShouldEqual, "python")
	})

	t.Run("different Prefix flag is treated as missing", func(t *testing.T) {
		desired := []ForwardCommandIntent{{Name: "go", Prefix: true}}
		active := []ForwardCommandIntent{{Name: "go", Prefix: false}}

		got := computeMissingForwardCommands(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].Name, test.ShouldEqual, "go")
		test.That(t, got[0].Prefix, test.ShouldBeTrue)
	})
}

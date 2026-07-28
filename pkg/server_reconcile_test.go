package graft

import (
	"context"
	"log/slog"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
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

	t.Run("active with different explicit modes is treated as missing", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "600", DefaultDirectoryMode: "700"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "644", DefaultDirectoryMode: "755"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].DefaultFileMode, test.ShouldEqual, "600")
	})

	t.Run("active with different SyncInclude is treated as missing", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", SyncInclude: []string{"**/*_pb2.py"}},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0].SyncInclude, test.ShouldResemble, []string{"**/*_pb2.py"})
	})

	t.Run("active with matching SyncInclude is skipped", func(t *testing.T) {
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", SyncInclude: []string{"**/*_pb2.py"}},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A", SyncInclude: []string{"**/*_pb2.py"}},
		}

		test.That(t, computeMissingSyncs(desired, active), test.ShouldBeEmpty)
	})

	t.Run("desired with empty modes matches active with explicit modes", func(t *testing.T) {
		// Empty modes mean "no opinion": a bare graft sync against a sync
		// whose modes were configured must not reset them.
		desired := []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
		}
		active := []SynchronizationIntent{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "640", DefaultDirectoryMode: "750"},
		}

		got := computeMissingSyncs(desired, active)
		test.That(t, got, test.ShouldBeEmpty)
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

func TestComputeMissingEnvForward(t *testing.T) {
	t.Run("nothing desired returns nothing", func(t *testing.T) {
		got := computeMissingEnvForward(nil, nil)
		test.That(t, got, test.ShouldBeEmpty)
	})

	t.Run("all desired returns all when none active", func(t *testing.T) {
		got := computeMissingEnvForward([]string{"FOO_API_KEY", "FOO_APP_KEY"}, nil)
		test.That(t, len(got), test.ShouldEqual, 2)
	})

	t.Run("exact match in active is skipped", func(t *testing.T) {
		desired := []string{"FOO_API_KEY", "FOO_APP_KEY"}
		active := []string{"FOO_API_KEY"}

		got := computeMissingEnvForward(desired, active)
		test.That(t, len(got), test.ShouldEqual, 1)
		test.That(t, got[0], test.ShouldEqual, "FOO_APP_KEY")
	})
}

func TestValidateEnvForwardNames(t *testing.T) {
	t.Run("plain names and globs are valid", func(t *testing.T) {
		test.That(t, validateEnvForwardNames([]string{"FOO_API_KEY", "FOO_*"}), test.ShouldBeNil)
	})

	t.Run("bare wildcard is rejected as too broad", func(t *testing.T) {
		err := validateEnvForwardNames([]string{"*"})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, errors.Is(err, errEnvForwardTooBroad), test.ShouldBeTrue)
	})

	t.Run("malformed glob pattern is rejected", func(t *testing.T) {
		err := validateEnvForwardNames([]string{"DD_["})
		test.That(t, err, test.ShouldNotBeNil)
	})
}

// TestServerUpdateEnvForwardDedup verifies that calling UpdateEnvForward
// twice with the same name is idempotent: ConnectionConfig.Validate rejects
// duplicate EnvForward entries, and rootConfig is validated on every
// persistConfig call (for every feature, not just this one), so a duplicate
// here would silently break config persistence entirely.
func TestServerUpdateEnvForwardDedup(t *testing.T) {
	// Must not touch the real daemon's state/socket: NewServer resolves its
	// socket path from GRAFT_STATE_HOME, and replace=true below would kill
	// whatever is currently listening there. mkShortTempDir (not t.TempDir)
	// because unix socket paths are capped at 104 chars on macOS.
	t.Setenv("GRAFT_STATE_HOME", mkShortTempDir(t, "ste-"))

	srv, err := NewServer(&RootConfig{
		Connections: []ConnectionConfig{{Name: "test-conn", Destination: "ssh://u@h"}},
	}, ServerRoleLocal, "", true, &BufferedLineWriter{MaxLines: 10}, "", slog.LevelDebug)
	test.That(t, err, test.ShouldBeNil)

	defer srv.Close()

	daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
	daemon.runCtx = context.Background()
	daemon.setState(ConnectionStateConnected)

	conn, connErr := srv.connMgr.createConnection("test-conn", "/local", "/remote", daemon, false)
	test.That(t, connErr, test.ShouldBeNil)

	test.That(t, srv.UpdateEnvForward([]string{"FOO_API_KEY"}, "test-conn"), test.ShouldBeNil)
	test.That(t, srv.UpdateEnvForward([]string{"FOO_API_KEY"}, "test-conn"), test.ShouldBeNil)

	test.That(t, conn.EnvForwardNames(), test.ShouldResemble, []string{"FOO_API_KEY"})

	found := false

	for _, cc := range srv.rootConfig.Connections {
		if cc.Name == "test-conn" {
			test.That(t, cc.EnvForward, test.ShouldResemble, []string{"FOO_API_KEY"})

			found = true
		}
	}

	test.That(t, found, test.ShouldBeTrue)

	// rootConfig as a whole must still validate: a duplicate here would
	// break persistConfig for every feature, not just env forwarding.
	test.That(t, srv.rootConfig.Validate(), test.ShouldBeNil)
}

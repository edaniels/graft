package graft

import (
	"testing"

	"go.viam.com/test"
)

func TestRootConfigSyncModesFor(t *testing.T) {
	conf := &RootConfig{
		Connections: []ConnectionConfig{{
			Name:        "conn",
			Destination: "ssh://u@h",
			Synchronizations: []SynchronizationIntentConfig{
				{FromLocal: "/plain", ToRemote: "/P"},
				{FromLocal: "/tight", ToRemote: "/T", DefaultFileMode: "600", DefaultDirectoryMode: "700"},
			},
		}},
	}

	t.Run("returns recorded modes for a known sync", func(t *testing.T) {
		fileMode, dirMode := conf.SyncModesFor("conn", "/tight")
		test.That(t, fileMode, test.ShouldEqual, "600")
		test.That(t, dirMode, test.ShouldEqual, "700")
	})

	t.Run("returns empty modes for a sync without them", func(t *testing.T) {
		fileMode, dirMode := conf.SyncModesFor("conn", "/plain")
		test.That(t, fileMode, test.ShouldBeEmpty)
		test.That(t, dirMode, test.ShouldBeEmpty)
	})

	t.Run("returns empty modes for unknown connection or sync", func(t *testing.T) {
		fileMode, dirMode := conf.SyncModesFor("other", "/tight")
		test.That(t, fileMode, test.ShouldBeEmpty)
		test.That(t, dirMode, test.ShouldBeEmpty)

		fileMode, dirMode = conf.SyncModesFor("conn", "/unknown")
		test.That(t, fileMode, test.ShouldBeEmpty)
		test.That(t, dirMode, test.ShouldBeEmpty)
	})
}

func TestConnectionConfigValidateSyncModes(t *testing.T) {
	base := ConnectionConfig{Name: "conn", Destination: "ssh://u@h"}

	t.Run("empty modes are valid", func(t *testing.T) {
		conf := base
		conf.Synchronizations = []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A"},
		}

		test.That(t, conf.Validate(), test.ShouldBeNil)
	})

	t.Run("valid explicit modes pass", func(t *testing.T) {
		conf := base
		conf.Synchronizations = []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "640", DefaultDirectoryMode: "750"},
		}

		test.That(t, conf.Validate(), test.ShouldBeNil)
	})

	t.Run("unparseable file mode fails", func(t *testing.T) {
		conf := base
		conf.Synchronizations = []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "banana"},
		}

		test.That(t, conf.Validate(), test.ShouldNotBeNil)
	})

	t.Run("file mode with exec bits fails", func(t *testing.T) {
		conf := base
		conf.Synchronizations = []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", DefaultFileMode: "755"},
		}

		test.That(t, conf.Validate(), test.ShouldNotBeNil)
	})

	t.Run("directory mode with non-permission bits fails", func(t *testing.T) {
		conf := base
		conf.Synchronizations = []SynchronizationIntentConfig{
			{FromLocal: "/a", ToRemote: "/A", DefaultDirectoryMode: "1777"},
		}

		test.That(t, conf.Validate(), test.ShouldNotBeNil)
	})
}

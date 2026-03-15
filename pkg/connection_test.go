package graft

import (
	"net/url"
	"testing"

	"go.viam.com/test"
)

func newTestConnectionWithRoots(localRoot, remoteRoot string) *Connection {
	daemon := newRemoteDaemon(&noopConnector{})

	return newConnection(daemon, "test", localRoot, remoteRoot)
}

func TestMatchCWD(t *testing.T) {
	t.Run("with LocalRoot and RemoteRoot", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/home/user/proj", "/remote/proj")

		// Exact match
		result, ok := conn.MatchCWD("/home/user/proj")
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, result, test.ShouldEqual, "/remote/proj")

		// Subdirectory
		result, ok = conn.MatchCWD("/home/user/proj/src/main")
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, result, test.ShouldEqual, "/remote/proj/src/main")

		// No match
		_, ok = conn.MatchCWD("/other/path")
		test.That(t, ok, test.ShouldBeFalse)

		// No match on partial directory name (proj vs project)
		_, ok = conn.MatchCWD("/home/user/project")
		test.That(t, ok, test.ShouldBeFalse)
	})

	t.Run("without RemoteRoot falls back to sync", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/home/user/proj", "")

		// No match since no RemoteRoot and no syncs
		_, ok := conn.MatchCWD("/home/user/proj/src")
		test.That(t, ok, test.ShouldBeFalse)
	})

	t.Run("without LocalRoot or RemoteRoot", func(t *testing.T) {
		conn := newTestConnectionWithRoots("", "")

		_, ok := conn.MatchCWD("/any/path")
		test.That(t, ok, test.ShouldBeFalse)
	})
}

func TestMatchCWDWithSyncFallback(t *testing.T) {
	t.Run("CWD inside project dir matches via localRoot/remoteRoot", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/home/user/arc/infra/anvil/cluster", "~/arc/infra/anvil/cluster")
		conn.synchronizations["/home/user/arc"] = activeSync{destination: "~/arc"}

		result, ok := conn.MatchCWD("/home/user/arc/infra/anvil/cluster/src")
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, result, test.ShouldEqual, "~/arc/infra/anvil/cluster/src")
	})

	t.Run("CWD inside workspace but outside project matches via sync entry", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/home/user/arc/infra/anvil/cluster", "~/arc/infra/anvil/cluster")
		conn.synchronizations["/home/user/arc"] = activeSync{destination: "~/arc"}

		result, ok := conn.MatchCWD("/home/user/arc/other/subdir")
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, result, test.ShouldEqual, "~/arc/other/subdir")
	})

	t.Run("CWD outside workspace gives no match", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/home/user/arc/infra/anvil/cluster", "~/arc/infra/anvil/cluster")
		conn.synchronizations["/home/user/arc"] = activeSync{destination: "~/arc"}

		_, ok := conn.MatchCWD("/home/user/other-project")
		test.That(t, ok, test.ShouldBeFalse)
	})
}

func TestSetRoots(t *testing.T) {
	t.Run("sets roots on connection without existing roots", func(t *testing.T) {
		conn := newTestConnectionWithRoots("", "")

		err := conn.SetRoots("/local/dir", "/remote/dir")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, conn.LocalRoot(), test.ShouldEqual, "/local/dir")
		test.That(t, conn.RemoteRoot(), test.ShouldEqual, "/remote/dir")
	})

	t.Run("sets only local root", func(t *testing.T) {
		conn := newTestConnectionWithRoots("", "")

		err := conn.SetRoots("/local/dir", "")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, conn.LocalRoot(), test.ShouldEqual, "/local/dir")
		test.That(t, conn.RemoteRoot(), test.ShouldEqual, "")
	})

	t.Run("updates roots when no syncs active", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/old/local", "/old/remote")

		err := conn.SetRoots("/new/local", "/new/remote")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, conn.LocalRoot(), test.ShouldEqual, "/new/local")
		test.That(t, conn.RemoteRoot(), test.ShouldEqual, "/new/remote")
	})

	t.Run("rejects update when syncs active", func(t *testing.T) {
		conn := newTestConnectionWithRoots("/old/local", "/old/remote")
		conn.synchronizations["/old/local"] = activeSync{destination: "/old/remote"}

		err := conn.SetRoots("/new/local", "/new/remote")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "active synchronization")
		// Roots unchanged
		test.That(t, conn.LocalRoot(), test.ShouldEqual, "/old/local")
		test.That(t, conn.RemoteRoot(), test.ShouldEqual, "/old/remote")
	})

	t.Run("rejects setting roots when syncs active even without prior roots", func(t *testing.T) {
		conn := newTestConnectionWithRoots("", "")
		conn.synchronizations["/some/path"] = activeSync{destination: "/remote/path"}

		err := conn.SetRoots("/local/dir", "/remote/dir")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "active synchronization")
	})
}

func TestDaemonKey(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		identity string
		wantKey  string
	}{
		{
			"basic ssh",
			"ssh://user@host",
			"id1",
			"user@host::id1",
		},
		{
			"ssh with default port stripped",
			"ssh://user@host:22",
			"id1",
			"user@host::id1",
		},
		{
			"ssh with non-default port kept",
			"ssh://user@host:2222",
			"id1",
			"user@host:2222::id1",
		},
		{
			"same host different users are different daemons",
			"ssh://other@host",
			"id1",
			"other@host::id1",
		},
		{
			"same host different identities are different daemons",
			"ssh://user@host",
			"id2",
			"user@host::id2",
		},
		{
			"no user",
			"ssh://host",
			"id1",
			"host::id1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.url)
			test.That(t, err, test.ShouldBeNil)
			test.That(t, daemonKey(u, tt.identity), test.ShouldEqual, tt.wantKey)
		})
	}
}

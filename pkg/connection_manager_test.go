package graft

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

func TestConnectionManagerConnectionsSnapshot(t *testing.T) {
	mgr := NewConnectionManager()
	defer mgr.Close()

	// Empty manager returns empty map.
	conns := mgr.Connections()
	test.That(t, conns, test.ShouldBeEmpty)

	// Add a connection directly to the map (simulating createConnection).
	daemon := newRemoteDaemon(&noopConnector{})
	daemon.runCtx = mgr.runCtx
	conn, err := mgr.createConnection("test-conn", "/local", "/remote", daemon, false)
	test.That(t, err, test.ShouldBeNil)

	// Connections() returns the connection.
	conns = mgr.Connections()
	test.That(t, len(conns), test.ShouldEqual, 1)
	test.That(t, conns["test-conn"], test.ShouldEqual, conn)

	// Returned map is an independent copy.
	delete(conns, "test-conn")

	conns2 := mgr.Connections()
	test.That(t, len(conns2), test.ShouldEqual, 1)
}

func TestConnectionManagerConnectionVisibleDuringInit(t *testing.T) {
	initStarted := make(chan struct{})
	initContinue := make(chan struct{})

	connector := &fakeInitConnector{
		initFunc: func(ctx context.Context) (bool, error) {
			close(initStarted)

			select {
			case <-initContinue:
				// Fail so we don't need full daemon setup.
				return false, errors.New("intentional fail")
			case <-ctx.Done():
				return false, errors.Wrap(context.Cause(ctx))
			}
		},
	}

	mgr := NewConnectionManager()
	defer mgr.Close()

	mgr.RegisterConnectorFactory("ssh", &fakeConnectorFactory{connector: connector})

	destURL, err := url.Parse("ssh://host")
	test.That(t, err, test.ShouldBeNil)

	initDone := make(chan error, 1)

	go func() {
		_, err := mgr.Restore(t.Context(), "myconn", destURL, "/local", "/remote", "", false)
		initDone <- err
	}()

	// Wait for transport init to start.
	<-initStarted

	// Connection should be visible in Initializing state.
	conns := mgr.Connections()
	test.That(t, len(conns), test.ShouldEqual, 1)
	conn := conns["myconn"]
	test.That(t, conn, test.ShouldNotBeNil)
	state, _ := conn.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateInitializing)

	// Let init finish (with failure).
	close(initContinue)
	<-initDone
}

func TestConnectionManagerFailedInitDestroyIfFailRemovesConnection(t *testing.T) {
	connector := &fakeInitConnector{
		initFunc: func(_ context.Context) (bool, error) {
			return false, errors.New("transport failed")
		},
	}

	mgr := NewConnectionManager()
	defer mgr.Close()

	mgr.RegisterConnectorFactory("ssh", &fakeConnectorFactory{connector: connector})

	destURL, err := url.Parse("ssh://host")
	test.That(t, err, test.ShouldBeNil)

	// Initialize (destroyIfFail=true) should fail and remove connection from map.
	_, initErr := mgr.Initialize(t.Context(), "myconn", destURL, "/local", "/remote", "", false)
	test.That(t, initErr, test.ShouldNotBeNil)

	conns := mgr.Connections()
	test.That(t, conns, test.ShouldBeEmpty)
}

func TestConnectionManagerFailedInitRestoreLeavesConnection(t *testing.T) {
	connector := &fakeInitConnector{
		initFunc: func(_ context.Context) (bool, error) {
			return false, errors.New("transport failed")
		},
	}

	mgr := NewConnectionManager()
	defer mgr.Close()

	mgr.RegisterConnectorFactory("ssh", &fakeConnectorFactory{connector: connector})

	destURL, err := url.Parse("ssh://host")
	test.That(t, err, test.ShouldBeNil)

	// Restore (destroyIfFail=false) should fail but leave connection in map.
	_, restoreErr := mgr.Restore(t.Context(), "myconn", destURL, "/local", "/remote", "", false)
	test.That(t, restoreErr, test.ShouldNotBeNil)

	conns := mgr.Connections()
	test.That(t, len(conns), test.ShouldEqual, 1)
	conn := conns["myconn"]
	test.That(t, conn, test.ShouldNotBeNil)
	state, _ := conn.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateFailed)
}

func TestConnectionManagerRemoveForwardCommands(t *testing.T) {
	t.Run("removes commands from an existing connection", func(t *testing.T) {
		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		daemon.setState(ConnectionStateConnected)
		conn, createErr := mgr.createConnection("test-conn", "/local", "/remote", daemon, false)
		test.That(t, createErr, test.ShouldBeNil)
		conn.UpdateForwardCommands([]ForwardCommandIntent{
			{Name: "go", Prefix: false},
			{Name: "python", Prefix: false},
		})

		err := mgr.RemoveForwardCommands("test-conn", []string{"go"})
		test.That(t, err, test.ShouldBeNil)

		intents := conn.ForwardIntents()
		test.That(t, len(intents), test.ShouldEqual, 1)
		test.That(t, intents[0].Name, test.ShouldEqual, "python")
	})

	t.Run("returns error for unknown connection", func(t *testing.T) {
		mgr := NewConnectionManager()
		defer mgr.Close()

		err := mgr.RemoveForwardCommands("nonexistent", []string{"go"})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, errors.Is(err, errConnectionNotFound), test.ShouldBeTrue)
	})
}

// fakeConnectorFactory implements ConnectorFactory for testing.
type fakeConnectorFactory struct {
	connector RemoteConnector
}

func (f *fakeConnectorFactory) CreateConnector(_ context.Context, _ *url.URL, _ string) (RemoteConnector, error) {
	return f.connector, nil
}

func TestWriteConnectionRootsFile(t *testing.T) {
	rootDir := t.TempDir()
	mgr := &ConnectionManager{
		connections:         map[string]*Connection{},
		connectionRootsPath: filepath.Join(rootDir, connectionRootsFileName),
	}

	t.Run("writes nothing when no connections", func(t *testing.T) {
		mgr.writeConnectionRootsFile()

		data, err := os.ReadFile(mgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "")
	})

	t.Run("writes local root and name for each connection", func(t *testing.T) {
		mgr.connections["dev"] = &Connection{name: "dev", localRoot: "/home/user/project"}
		mgr.writeConnectionRootsFile()

		data, err := os.ReadFile(mgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "/home/user/project\tdev\n")
	})

	t.Run("skips connections with no local root", func(t *testing.T) {
		mgr.connections["no-root"] = &Connection{name: "no-root", localRoot: ""}
		mgr.writeConnectionRootsFile()

		data, err := os.ReadFile(mgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "/home/user/project\tdev\n")
	})

	t.Run("skips write when content unchanged", func(t *testing.T) {
		// Remove connection without local root to get stable state
		delete(mgr.connections, "no-root")
		mgr.writeConnectionRootsFile()

		info1, err := os.Stat(mgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)

		mgr.writeConnectionRootsFile()

		info2, err := os.Stat(mgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info2.ModTime(), test.ShouldEqual, info1.ModTime())
	})

	t.Run("empty path is a no-op", func(_ *testing.T) {
		noPathMgr := &ConnectionManager{
			connections:         map[string]*Connection{},
			connectionRootsPath: "",
		}
		noPathMgr.connections["x"] = &Connection{name: "x", localRoot: "/tmp/x"}
		// Should not panic or error
		noPathMgr.writeConnectionRootsFile()
	})

	t.Run("resolves symlinks in local roots", func(t *testing.T) {
		symlinkDir := t.TempDir()
		realDir := filepath.Join(symlinkDir, "real")
		test.That(t, os.Mkdir(realDir, DirPerms), test.ShouldBeNil)

		linkPath := filepath.Join(symlinkDir, "link")
		test.That(t, os.Symlink(realDir, linkPath), test.ShouldBeNil)

		symlinkMgr := &ConnectionManager{
			connections:         map[string]*Connection{},
			connectionRootsPath: filepath.Join(symlinkDir, connectionRootsFileName),
		}
		symlinkMgr.connections["linked"] = &Connection{name: "linked", localRoot: linkPath}
		symlinkMgr.writeConnectionRootsFile()

		data, err := os.ReadFile(symlinkMgr.connectionRootsPath)
		test.That(t, err, test.ShouldBeNil)

		// The written path should be the resolved real path, not the symlink.
		resolved, err := filepath.EvalSymlinks(linkPath)
		test.That(t, err, test.ShouldBeNil)
		resolved, err = filepath.Abs(resolved)
		test.That(t, err, test.ShouldBeNil)

		test.That(t, string(data), test.ShouldEqual, resolved+"\tlinked\n")
	})
}

func TestRefreshConnectionRootsFile(t *testing.T) {
	rootDir := t.TempDir()
	mgr := &ConnectionManager{
		connections:         map[string]*Connection{},
		connectionRootsPath: filepath.Join(rootDir, connectionRootsFileName),
	}
	mgr.connections["dev"] = &Connection{name: "dev", localRoot: "/home/user/project"}
	mgr.RefreshConnectionRootsFile()

	data, err := os.ReadFile(mgr.connectionRootsPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "/home/user/project\tdev\n")
}

func TestRunRecreatesConnectionRootsFile(t *testing.T) {
	rootDir := t.TempDir()
	runCtx, runCtxCancel := context.WithCancel(context.Background())

	mgr := &ConnectionManager{
		connections:         map[string]*Connection{},
		daemons:             map[string]*remoteDaemon{},
		schemes:             map[string]ConnectorFactory{},
		connectionRootsPath: filepath.Join(rootDir, connectionRootsFileName),
		runCtx:              runCtx,
		runCtxCancel:        runCtxCancel,
	}
	defer mgr.Close()

	daemon := newRemoteDaemon(&noopConnector{})
	daemon.runCtx = runCtx
	mgr.connections["dev"] = newConnection(daemon, "dev", "/home/user/project", "/remote", false)

	// Write the file initially.
	mgr.connMgrMu.Lock()
	mgr.writeConnectionRootsFile()
	mgr.connMgrMu.Unlock()

	_, err := os.Stat(mgr.connectionRootsPath)
	test.That(t, err, test.ShouldBeNil)

	// Delete the file to simulate external removal.
	test.That(t, os.Remove(mgr.connectionRootsPath), test.ShouldBeNil)
	_, err = os.Stat(mgr.connectionRootsPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)

	// Run one tick: the file should be recreated.
	mgr.tick(runCtx)

	data, err := os.ReadFile(mgr.connectionRootsPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "/home/user/project\tdev\n")
}

func TestConnectionByCWDSkipsBackground(t *testing.T) {
	localRoot := t.TempDir()
	subDir := filepath.Join(localRoot, "src")
	test.That(t, os.MkdirAll(subDir, DirPerms), test.ShouldBeNil)

	mgr := &ConnectionManager{
		connections: map[string]*Connection{},
	}
	mgr.connections["bg"] = &Connection{name: "bg", localRoot: localRoot, background: true}

	_, ok := mgr.matchConnectionByCWD(context.Background(), subDir)
	test.That(t, ok, test.ShouldBeFalse)
}

func TestConnectionByCWDBackgroundStillExplicitlyUsable(t *testing.T) {
	mgr := NewConnectionManager()
	defer mgr.Close()

	daemon := newRemoteDaemon(&noopConnector{})
	daemon.runCtx = mgr.runCtx
	daemon.setState(ConnectionStateConnected)
	_, createErr := mgr.createConnection("bg-conn", "/local", "/remote", daemon, true)
	test.That(t, createErr, test.ShouldBeNil)

	conn, err := mgr.Connection("bg-conn")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, conn.Name(), test.ShouldEqual, "bg-conn")
	test.That(t, conn.Background(), test.ShouldBeTrue)
}

func TestWriteConnectionRootsFileExcludesBackground(t *testing.T) {
	rootDir := t.TempDir()
	mgr := &ConnectionManager{
		connections:         map[string]*Connection{},
		connectionRootsPath: filepath.Join(rootDir, connectionRootsFileName),
	}

	mgr.connections["fg"] = &Connection{name: "fg", localRoot: "/home/user/project"}
	mgr.connections["bg"] = &Connection{name: "bg", localRoot: "/home/user/monorepo", background: true}
	mgr.writeConnectionRootsFile()

	data, err := os.ReadFile(mgr.connectionRootsPath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "/home/user/project\tfg\n")
}

func TestConnectionByCWD(t *testing.T) {
	t.Run("single connection matches", func(t *testing.T) {
		localRoot := t.TempDir()
		subDir := filepath.Join(localRoot, "src")
		test.That(t, os.MkdirAll(subDir, DirPerms), test.ShouldBeNil)

		mgr := &ConnectionManager{
			connections: map[string]*Connection{},
		}
		mgr.connections["dev"] = &Connection{name: "dev", localRoot: localRoot}

		conn, ok := mgr.matchConnectionByCWD(context.Background(), subDir)
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, conn.Name(), test.ShouldEqual, "dev")
	})

	t.Run("multiple matches returns false", func(t *testing.T) {
		wsRoot := t.TempDir()
		projRoot := filepath.Join(wsRoot, "infra", "anvil")
		projSubDir := filepath.Join(projRoot, "src")
		test.That(t, os.MkdirAll(projSubDir, DirPerms), test.ShouldBeNil)

		mgr := &ConnectionManager{
			connections: map[string]*Connection{},
		}
		mgr.connections["workspace"] = &Connection{name: "workspace", localRoot: wsRoot}
		mgr.connections["anvil"] = &Connection{name: "anvil", localRoot: projRoot}

		// Both connections match projSubDir and should refuse to auto-select.
		_, ok := mgr.matchConnectionByCWD(context.Background(), projSubDir)
		test.That(t, ok, test.ShouldBeFalse)
	})

	t.Run("no match when cwd is outside all roots", func(t *testing.T) {
		localRoot := t.TempDir()
		otherDir := t.TempDir()

		mgr := &ConnectionManager{
			connections: map[string]*Connection{},
		}
		mgr.connections["dev"] = &Connection{name: "dev", localRoot: localRoot}

		_, ok := mgr.matchConnectionByCWD(context.Background(), otherDir)
		test.That(t, ok, test.ShouldBeFalse)
	})

	t.Run("empty cwd returns no match", func(t *testing.T) {
		mgr := &ConnectionManager{
			connections: map[string]*Connection{},
		}
		mgr.connections["dev"] = &Connection{name: "dev", localRoot: "/some/path"}

		_, ok := mgr.matchConnectionByCWD(context.Background(), "")
		test.That(t, ok, test.ShouldBeFalse)
	})
}

func TestCreateConnectionRejectsOverlappingRoots(t *testing.T) {
	t.Run("new root is parent of existing", func(t *testing.T) {
		parentRoot := t.TempDir()
		childRoot := filepath.Join(parentRoot, "child")
		test.That(t, os.MkdirAll(childRoot, DirPerms), test.ShouldBeNil)

		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		_, err := mgr.createConnection("child-conn", childRoot, "/remote", daemon, false)
		test.That(t, err, test.ShouldBeNil)

		_, err = mgr.createConnection("parent-conn", parentRoot, "/remote2", daemon, false)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "overlaps")
	})

	t.Run("new root is child of existing", func(t *testing.T) {
		parentRoot := t.TempDir()
		childRoot := filepath.Join(parentRoot, "child")
		test.That(t, os.MkdirAll(childRoot, DirPerms), test.ShouldBeNil)

		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		_, err := mgr.createConnection("parent-conn", parentRoot, "/remote", daemon, false)
		test.That(t, err, test.ShouldBeNil)

		_, err = mgr.createConnection("child-conn", childRoot, "/remote2", daemon, false)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "overlaps")
	})

	t.Run("background connections do not conflict", func(t *testing.T) {
		parentRoot := t.TempDir()
		childRoot := filepath.Join(parentRoot, "child")
		test.That(t, os.MkdirAll(childRoot, DirPerms), test.ShouldBeNil)

		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		_, err := mgr.createConnection("fg-conn", childRoot, "/remote", daemon, false)
		test.That(t, err, test.ShouldBeNil)

		_, err = mgr.createConnection("bg-conn", parentRoot, "/remote2", daemon, true)
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("non-overlapping roots are fine", func(t *testing.T) {
		rootA := t.TempDir()
		rootB := t.TempDir()

		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		_, err := mgr.createConnection("connA", rootA, "/remote", daemon, false)
		test.That(t, err, test.ShouldBeNil)

		_, err = mgr.createConnection("connB", rootB, "/remote2", daemon, false)
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("empty root never conflicts", func(t *testing.T) {
		root := t.TempDir()

		mgr := NewConnectionManager()
		defer mgr.Close()

		daemon := newRemoteDaemon(&noopConnector{})
		daemon.runCtx = mgr.runCtx
		_, err := mgr.createConnection("connA", root, "/remote", daemon, false)
		test.That(t, err, test.ShouldBeNil)

		_, err = mgr.createConnection("connB", "", "/remote2", daemon, false)
		test.That(t, err, test.ShouldBeNil)
	})
}

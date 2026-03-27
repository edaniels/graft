package graft

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/test"
)

func TestWriteSessionConnectionFile(t *testing.T) {
	sessDir := t.TempDir()
	shimPath := filepath.Join(sessDir, "shims")
	test.That(t, os.MkdirAll(shimPath, DirPerms), test.ShouldBeNil)
	sess := &Session{dir: sessDir, shimPath: shimPath}
	mgr := &SessionManager{}

	t.Run("writes connection name to correct path", func(t *testing.T) {
		mgr.writeSessionConnectionFile(sess, "dev-server")

		data, err := os.ReadFile(filepath.Join(sessDir, currentConnectionFileName))
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "dev-server")
	})

	t.Run("writes empty string when no connection", func(t *testing.T) {
		mgr.writeSessionConnectionFile(sess, "")

		data, err := os.ReadFile(filepath.Join(sessDir, currentConnectionFileName))
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "")
	})

	t.Run("skips write when content unchanged", func(t *testing.T) {
		filePath := filepath.Join(sessDir, currentConnectionFileName)

		mgr.writeSessionConnectionFile(sess, "same-name")

		info1, err := os.Stat(filePath)
		test.That(t, err, test.ShouldBeNil)

		mgr.writeSessionConnectionFile(sess, "same-name")

		info2, err := os.Stat(filePath)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info2.ModTime(), test.ShouldEqual, info1.ModTime())
	})

	t.Run("updates when connection name changes", func(t *testing.T) {
		mgr.writeSessionConnectionFile(sess, "old-conn")

		data, err := os.ReadFile(filepath.Join(sessDir, currentConnectionFileName))
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "old-conn")

		mgr.writeSessionConnectionFile(sess, "new-conn")

		data, err = os.ReadFile(filepath.Join(sessDir, currentConnectionFileName))
		test.That(t, err, test.ShouldBeNil)
		test.That(t, string(data), test.ShouldEqual, "new-conn")
	})
}

// TODO(erd): unify with newConnection so tests exercise the real creation path.
func newTestConnection(name, localRoot string) *Connection {
	return &Connection{
		name:      name,
		localRoot: localRoot,
		daemon:    &remoteDaemon{state: ConnectionStateConnected},
	}
}

// TODO(erd): unify with NewSessionManager/NewConnectionManager so tests exercise real construction.
func newTestSessionManager(t *testing.T, conns map[string]*Connection) (*SessionManager, string) {
	t.Helper()

	connMgr := &ConnectionManager{
		connections:         conns,
		connectionRootsPath: filepath.Join(t.TempDir(), connectionRootsFileName),
	}

	sessionsRoot, err := SessionsRoot()
	test.That(t, err, test.ShouldBeNil)

	mgr := &SessionManager{
		sessions: map[uint64]*Session{},
		connMgr:  connMgr,
		rootPath: sessionsRoot,
	}

	return mgr, sessionsRoot
}

func TestUpdateSessionCWDReconciles(t *testing.T) {
	localRoot := t.TempDir()

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"myconn": newTestConnection("myconn", localRoot),
	})

	// Use a PID unlikely to conflict with real processes.
	pid := uint64(99999)

	// Clean up the session directory after the test.
	sessPath := SessionPathFromRoot(sessionsRoot, "99999")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	// Call UpdateSessionCWD with a CWD inside the connection's local root.
	ctx := context.Background()
	err := mgr.UpdateSessionCWD(ctx, pid, localRoot)
	test.That(t, err, test.ShouldBeNil)

	// The current_connection file should be written immediately (no tick needed).
	data, readErr := os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "myconn")
}

func TestSelectConnectionUsesPin(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": newTestConnection("connA", rootA),
		"connB": newTestConnection("connB", rootB),
	})

	pid := uint64(99990)
	sessPath := SessionPathFromRoot(sessionsRoot, "99990")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	// CWD inside rootA normally resolves to connA.
	err := mgr.UpdateSessionCWD(ctx, pid, rootA)
	test.That(t, err, test.ShouldBeNil)

	sess, err := mgr.SessionByPID(pid)
	test.That(t, err, test.ShouldBeNil)

	conn, err := mgr.selectConnection(ctx, sess, "", rootA)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, conn.Name(), test.ShouldEqual, "connA")

	// Pin to connB - should override CWD.
	sess.SetPinnedConnection("connB")
	conn, err = mgr.selectConnection(ctx, sess, "", rootA)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, conn.Name(), test.ShouldEqual, "connB")
}

func TestSelectConnectionClearPin(t *testing.T) {
	rootA := t.TempDir()

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": newTestConnection("connA", rootA),
		"connB": newTestConnection("connB", t.TempDir()),
	})

	pid := uint64(99989)
	sessPath := SessionPathFromRoot(sessionsRoot, "99989")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()
	err := mgr.UpdateSessionCWD(ctx, pid, rootA)
	test.That(t, err, test.ShouldBeNil)

	sess, err := mgr.SessionByPID(pid)
	test.That(t, err, test.ShouldBeNil)

	// Pin to connB, then clear.
	sess.SetPinnedConnection("connB")
	sess.SetPinnedConnection("")

	conn, err := mgr.selectConnection(ctx, sess, "", rootA)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, conn.Name(), test.ShouldEqual, "connA")
}

func TestPinConnectionValidatesExists(t *testing.T) {
	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{})

	pid := uint64(99988)
	sessPath := SessionPathFromRoot(sessionsRoot, "99988")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	_, err := mgr.PinConnection(ctx, pid, "nonexistent")
	test.That(t, err, test.ShouldNotBeNil)
}

func TestTickReconcileSessionWithPin(t *testing.T) {
	rootA := t.TempDir()

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": newTestConnection("connA", rootA),
		"connB": newTestConnection("connB", t.TempDir()),
	})

	pid := uint64(99987)
	sessPath := SessionPathFromRoot(sessionsRoot, "99987")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	// Set CWD inside rootA.
	err := mgr.UpdateSessionCWD(ctx, pid, rootA)
	test.That(t, err, test.ShouldBeNil)

	// Without pin, current_connection reflects CWD match.
	data, readErr := os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "connA")

	// Pin to connB and reconcile.
	_, err = mgr.PinConnection(ctx, pid, "connB")
	test.That(t, err, test.ShouldBeNil)

	data, readErr = os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "connB")
}

func TestResolveSessionConnectionRespectsPin(t *testing.T) {
	rootA := t.TempDir()

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": newTestConnection("connA", rootA),
		"connB": newTestConnection("connB", t.TempDir()),
	})

	pid := uint64(99985)
	sessPath := SessionPathFromRoot(sessionsRoot, "99985")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	// CWD inside rootA normally resolves to connA.
	err := mgr.UpdateSessionCWD(ctx, pid, rootA)
	test.That(t, err, test.ShouldBeNil)

	sess, err := mgr.SessionByPID(pid)
	test.That(t, err, test.ShouldBeNil)

	conn, ok := mgr.resolveSessionConnection(ctx, sess)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, conn.Name(), test.ShouldEqual, "connA")

	// Pin to connB - resolveSessionConnection should return connB.
	sess.SetPinnedConnection("connB")

	conn, ok = mgr.resolveSessionConnection(ctx, sess)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, conn.Name(), test.ShouldEqual, "connB")
}

func TestDesiredForwardingsForSessionRespectsPin(t *testing.T) {
	rootA := t.TempDir()

	connA := newTestConnection("connA", rootA)
	connB := newTestConnection("connB", t.TempDir())

	// Register a non-global forward on connB.
	connB.UpdateForwardCommands([]ForwardCommandIntent{
		{Name: "go", Prefix: false, Global: false},
	})

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": connA,
		"connB": connB,
	})

	pid := uint64(99984)
	sessPath := SessionPathFromRoot(sessionsRoot, "99984")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	// CWD is inside rootA, so CWD-based resolution would pick connA.
	err := mgr.UpdateSessionCWD(ctx, pid, rootA)
	test.That(t, err, test.ShouldBeNil)

	sess, err := mgr.SessionByPID(pid)
	test.That(t, err, test.ShouldBeNil)

	// Pin to connB.
	sess.SetPinnedConnection("connB")

	// Resolve the session connection (respects pin).
	resolvedConn, ok := mgr.resolveSessionConnection(ctx, sess)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, resolvedConn.Name(), test.ShouldEqual, "connB")

	// DesiredForwardingsForSession should include connB's "go" forward
	// because the session is pinned to connB, even though CWD is in rootA.
	fwds := mgr.DesiredForwardingsForSession(ctx, sess, resolvedConn)
	test.That(t, fwds, test.ShouldNotBeEmpty)

	connBFwds := fwds["connB"]
	test.That(t, connBFwds, test.ShouldNotBeNil)

	var found bool

	for _, intent := range connBFwds {
		if intent.Name == "go" {
			found = true

			break
		}
	}

	test.That(t, found, test.ShouldBeTrue)
}

func TestUpdateSessionCWDReconcileMultipleConnections(t *testing.T) {
	wsRoot := t.TempDir()
	projARoot := filepath.Join(wsRoot, "infra", "projectA")
	projBRoot := filepath.Join(wsRoot, "infra", "projectB")

	test.That(t, os.MkdirAll(projARoot, DirPerms), test.ShouldBeNil)
	test.That(t, os.MkdirAll(projBRoot, DirPerms), test.ShouldBeNil)

	mgr, sessionsRoot := newTestSessionManager(t, map[string]*Connection{
		"connA": newTestConnection("connA", projARoot),
		"connB": newTestConnection("connB", projBRoot),
	})

	pid := uint64(99998)
	sessPath := SessionPathFromRoot(sessionsRoot, "99998")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	ctx := context.Background()

	// CWD inside projectA should resolve to connA.
	err := mgr.UpdateSessionCWD(ctx, pid, projARoot)
	test.That(t, err, test.ShouldBeNil)

	data, readErr := os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "connA")

	// CWD inside projectB should resolve to connB.
	err = mgr.UpdateSessionCWD(ctx, pid, projBRoot)
	test.That(t, err, test.ShouldBeNil)

	data, readErr = os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "connB")
}

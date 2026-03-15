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

func TestUpdateSessionCWDReconciles(t *testing.T) {
	localRoot := t.TempDir()

	connMgr := &ConnectionManager{
		connections:         map[string]*Connection{},
		connectionRootsPath: filepath.Join(t.TempDir(), connectionRootsFileName),
	}
	connMgr.connections["myconn"] = &Connection{
		name:      "myconn",
		localRoot: localRoot,
		daemon:    &remoteDaemon{state: ConnectionStateConnected},
	}

	sessionsRoot, err := SessionsRoot()
	test.That(t, err, test.ShouldBeNil)

	mgr := &SessionManager{
		sessions: map[uint64]*Session{},
		connMgr:  connMgr,
		rootPath: sessionsRoot,
	}

	// Use a PID unlikely to conflict with real processes.
	pid := uint64(99999)

	// Clean up the session directory after the test.
	sessPath := SessionPathFromRoot(sessionsRoot, "99999")

	t.Cleanup(func() { os.RemoveAll(sessPath) })

	// Call UpdateSessionCWD with a CWD inside the connection's local root.
	ctx := context.Background()
	err = mgr.UpdateSessionCWD(ctx, pid, localRoot)
	test.That(t, err, test.ShouldBeNil)

	// The current_connection file should be written immediately (no tick needed).
	data, readErr := os.ReadFile(filepath.Join(sessPath, currentConnectionFileName))
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "myconn")
}

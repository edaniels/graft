package graft

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// echoConnector answers the "echo <path>" one-shot EstablishSynchronization
// uses to resolve the remote sync root; everything else is a noop.
type echoConnector struct {
	noopConnector
}

func (e *echoConnector) RunOneShotCommand(_ context.Context, cmd string) (string, error) {
	if after, ok := strings.CutPrefix(cmd, "echo "); ok {
		return after + "\n", nil
	}

	return "", nil
}

// fakeRemoteDaemonConn satisfies RemoteDaemonConnection with a gRPC client
// conn that never dials (passthrough to a nonexistent target).
type fakeRemoteDaemonConn struct {
	cc *grpc.ClientConn
}

func (f *fakeRemoteDaemonConn) ClientConn() *grpc.ClientConn { return f.cc }

func (f *fakeRemoteDaemonConn) Close() error { return errors.Wrap(f.cc.Close()) }

// stubEndpoint is a mutagen synchronization.Endpoint that goes nowhere. Poll
// blocks for the lifetime of the session; every other method errors so the
// controller parks the session in an error state instead of spinning.
type stubEndpoint struct{}

func (s *stubEndpoint) Poll(ctx context.Context) error {
	<-ctx.Done()

	return nil
}

//nolint:revive // mutagen's Endpoint interface puts error before the retry flag
func (s *stubEndpoint) Scan(context.Context, *core.Entry, bool) (*core.Snapshot, error, bool) {
	return nil, errors.New("stub endpoint cannot scan"), false
}

func (s *stubEndpoint) Stage([]string, [][]byte) ([]string, []*rsync.Signature, rsync.Receiver, error) {
	return nil, nil, nil, errors.New("stub endpoint cannot stage")
}

func (s *stubEndpoint) Supply([]string, []*rsync.Signature, rsync.Receiver) error {
	return errors.New("stub endpoint cannot supply")
}

func (s *stubEndpoint) Transition(context.Context, []*core.Change) ([]*core.Entry, []*core.Problem, bool, error) {
	return nil, nil, false, errors.New("stub endpoint cannot transition")
}

func (s *stubEndpoint) Shutdown() error { return nil }

// stubSyncProtocolHandler vends stubEndpoints for graft's sync protocol slot.
type stubSyncProtocolHandler struct{}

func (stubSyncProtocolHandler) Connect(
	context.Context,
	*logging.Logger,
	*urlpkg.URL,
	string,
	string,
	synchronization.Version,
	*synchronization.Configuration,
	bool,
) (synchronization.Endpoint, error) {
	return &stubEndpoint{}, nil
}

// registerStubSyncProtocolHandler installs the stub in graft's sync protocol
// slot (so session creation succeeds without a live remote daemon) and
// restores the previous handler on cleanup.
func registerStubSyncProtocolHandler(t *testing.T) {
	t.Helper()

	proto := urlpkg.Protocol(syncProtoNum)
	prev, had := synchronization.ProtocolHandlers[proto]
	synchronization.ProtocolHandlers[proto] = stubSyncProtocolHandler{}

	t.Cleanup(func() {
		if had {
			synchronization.ProtocolHandlers[proto] = prev
		} else {
			delete(synchronization.ProtocolHandlers, proto)
		}
	})
}

// newSyncTestConn returns a Connection wired with enough fakes for
// EstablishSynchronization to create real mutagen sessions against a temp
// mutagen data dir, plus the sync manager to inspect them with.
func newSyncTestConn(t *testing.T, localRoot string) (*Connection, *synchronization.Manager) {
	t.Helper()

	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())

	mgr, err := synchronization.NewManagerWithoutPersistence(logging.NewLoggerOnSlogger(slog.Default()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(mgr.Shutdown)

	registerStubSyncProtocolHandler(t)

	daemon := newRemoteDaemon(&echoConnector{}, slog.LevelDebug)

	cc, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { cc.Close() })

	daemon.remoteConn = &fakeRemoteDaemonConn{cc: cc}

	return newConnection(daemon, "test", localRoot, "/remote", false), mgr
}

func sessionNames(t *testing.T, mgr *synchronization.Manager) []string {
	t.Helper()

	_, states, err := mgr.List(context.Background(), &selection.Selection{All: true}, 0)
	test.That(t, err, test.ShouldBeNil)

	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, state.GetSession().GetName())
	}

	return names
}

func TestEstablishSynchronizationCanonicalizesFromLocal(t *testing.T) {
	localRoot := t.TempDir()
	conn, mgr := newSyncTestConn(t, localRoot)

	// A relative source must resolve against the connection's local root, not
	// the daemon's cwd (which is typically /: resolving "." there once caused
	// a sync of the entire filesystem).
	shadowed, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
		FromLocal: ".",
		ToRemote:  "/remote/x",
	}, mgr)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, shadowed, test.ShouldBeEmpty)

	// The active sync is keyed by the canonical absolute path.
	syncs := conn.Synchronizations()
	test.That(t, len(syncs), test.ShouldEqual, 1)
	test.That(t, syncs[0].FromLocal, test.ShouldEqual, localRoot)
	test.That(t, syncs[0].ToRemote, test.ShouldEqual, "/remote/x")

	// The mutagen session carries the absolute path and the canonical name.
	_, states, err := mgr.List(t.Context(), &selection.Selection{All: true}, 0)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(states), test.ShouldEqual, 1)
	test.That(t, states[0].GetSession().GetAlpha().GetPath(), test.ShouldEqual, localRoot)
	test.That(t, states[0].GetSession().GetName(), test.ShouldEqual,
		syncSessionName("test", SynchronizationIntent{FromLocal: localRoot, ToRemote: "/remote/x"}))

	// Re-establishing with the absolute spelling of the same tree is a no-op:
	// no duplicate session for one tree.
	_, err = conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
		FromLocal: localRoot,
		ToRemote:  "/remote/x",
	}, mgr)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, sessionNames(t, mgr), test.ShouldHaveLength, 1)
}

func TestEstablishSynchronizationRejectsRelativeWithoutLocalRoot(t *testing.T) {
	conn, mgr := newSyncTestConn(t, "")

	_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
		FromLocal: ".",
		ToRemote:  "/remote/x",
	}, mgr)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "no local root")
}

func TestEstablishSynchronizationRejectsFilesystemRoot(t *testing.T) {
	conn, mgr := newSyncTestConn(t, t.TempDir())

	_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
		FromLocal: "/",
		ToRemote:  "/remote/x",
	}, mgr)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "filesystem root")
}

func TestEstablishSynchronizationRejectsOverlap(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")

	test.That(t, os.MkdirAll(repo, DirPerms), test.ShouldBeNil)
	test.That(t, os.MkdirAll(other, DirPerms), test.ShouldBeNil)

	conn, mgr := newSyncTestConn(t, base)

	// Seed an active sync directly; the guard must fire before any remote
	// work happens. Overlapping syncs are rejected (rather than warned about)
	// because two sessions over one tree double-watch it and, worse, can
	// fight over content: a two-way session containing a .git tree battles
	// the one-way .git replica session managing the same directory.
	conn.synchronizations[repo] = activeSync{destination: "/remote/repo", syncGit: true}

	t.Run("child of an active sync is rejected", func(t *testing.T) {
		_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
			FromLocal: filepath.Join(repo, "sub"),
			ToRemote:  "/remote/sub",
		}, mgr)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "overlaps")
	})

	t.Run("parent of an active sync is rejected", func(t *testing.T) {
		_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
			FromLocal: base,
			ToRemote:  "/remote/base",
		}, mgr)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "overlaps")
	})

	t.Run("git dir of an active sync is rejected", func(t *testing.T) {
		_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
			FromLocal: filepath.Join(repo, ".git"),
			ToRemote:  "/remote/git",
		}, mgr)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "overlaps")
	})

	t.Run("non-overlapping sync is allowed", func(t *testing.T) {
		_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
			FromLocal: other,
			ToRemote:  "/remote/other",
		}, mgr)
		test.That(t, err, test.ShouldBeNil)
	})
}

func TestEstablishSessionRejectsRelativeAlphaPath(t *testing.T) {
	conn, mgr := newSyncTestConn(t, t.TempDir())

	// Defense in depth: even if a caller bypasses EstablishSynchronization's
	// canonicalization, a relative alpha path must never reach mutagen.
	_, err := conn.establishSession(t.Context(), mgr, "testsession", "./relative", "/remote/x",
		&synchronization.Configuration{}, &synchronization.Configuration{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "not absolute")
}

func TestSyncFilesToConnectionResolvesRelativeSourceDir(t *testing.T) {
	localRoot := t.TempDir()

	connMgr := NewConnectionManager(slog.LevelDebug)
	t.Cleanup(connMgr.Close)

	daemon := newRemoteDaemon(&echoConnector{}, slog.LevelDebug)
	daemon.runCtx = connMgr.runCtx
	daemon.setState(ConnectionStateConnected)

	cc, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { cc.Close() })

	daemon.remoteConn = &fakeRemoteDaemonConn{cc: cc}

	conn, err := connMgr.createConnection("c", localRoot, "/remote", daemon, false)
	test.That(t, err, test.ShouldBeNil)

	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())

	syncMgr, err := synchronization.NewManagerWithoutPersistence(logging.NewLoggerOnSlogger(slog.Default()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(syncMgr.Shutdown)

	registerStubSyncProtocolHandler(t)

	srv := &Server{
		connMgr: connMgr,
		rootConfig: &RootConfig{Connections: []ConnectionConfig{{
			Name:       "c",
			LocalRoot:  localRoot,
			RemoteRoot: "/remote",
		}}},
		synchronizationManager: syncMgr,
	}

	_, err = srv.SyncFilesToConnection(t.Context(), &graftv1.SyncFilesToConnectionRequest{
		ToConnectionName: "c",
		SourceDir:        ".",
		DestDir:          "/remote/x",
	})
	test.That(t, err, test.ShouldBeNil)

	syncs := conn.Synchronizations()
	test.That(t, len(syncs), test.ShouldEqual, 1)
	test.That(t, syncs[0].FromLocal, test.ShouldEqual, localRoot)

	// The persisted config intent is canonical too, so restarts find the same
	// session instead of creating a differently-named duplicate.
	test.That(t, len(srv.rootConfig.Connections), test.ShouldEqual, 1)
	test.That(t, srv.rootConfig.Connections[0].Synchronizations[0].FromLocal, test.ShouldEqual, localRoot)
}

func TestSyncFilesToConnectionRejectsRelativeSourceDirWithoutLocalRoot(t *testing.T) {
	connMgr := NewConnectionManager(slog.LevelDebug)
	t.Cleanup(connMgr.Close)

	daemon := newRemoteDaemon(&echoConnector{}, slog.LevelDebug)
	daemon.runCtx = connMgr.runCtx
	daemon.setState(ConnectionStateConnected)

	_, err := connMgr.createConnection("c", "", "/remote", daemon, false)
	test.That(t, err, test.ShouldBeNil)

	srv := &Server{
		connMgr:    connMgr,
		rootConfig: &RootConfig{},
	}

	_, err = srv.SyncFilesToConnection(t.Context(), &graftv1.SyncFilesToConnectionRequest{
		ToConnectionName: "c",
		SourceDir:        ".",
		DestDir:          "/remote/x",
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "no local root")
}

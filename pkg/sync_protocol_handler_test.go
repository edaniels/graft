package graft

import (
	"log/slog"
	"testing"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
)

func TestMutagenSyncProtocolHandlerResolvesByURLHost(t *testing.T) {
	// Mutagen's controller runs Connect on context.Background() (see
	// controller.run), so no context value can pick the connection; the
	// handler must resolve by the beta URL's host. That is also correct when
	// the named connection differs from the one the creator had in mind.
	var gotName string

	sentinel := errors.New("no transport yet")
	handler := &mutagenSyncProtocolHandler{
		resolveConn: func(name string) (*grpc.ClientConn, error) {
			gotName = name

			return nil, sentinel
		},
	}

	_, err := handler.Connect(
		t.Context(),
		nil,
		&urlpkg.URL{Protocol: urlpkg.Protocol(syncProtoNum), Host: "other-conn", Path: "/remote/x"},
		"",
		"session-id",
		synchronization.DefaultVersion,
		&synchronization.Configuration{},
		false,
	)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "no transport yet")
	test.That(t, gotName, test.ShouldEqual, "other-conn")
}

func TestMutagenSyncProtocolHandlerPanicsOnForeignURL(t *testing.T) {
	handler := &mutagenSyncProtocolHandler{
		resolveConn: func(string) (*grpc.ClientConn, error) { return nil, errors.New("unused") },
	}

	defer func() {
		test.That(t, recover(), test.ShouldNotBeNil)
	}()

	_, err := handler.Connect(
		t.Context(),
		nil,
		&urlpkg.URL{Protocol: urlpkg.Protocol_Local, Path: "/tmp/x"},
		"",
		"session-id",
		synchronization.DefaultVersion,
		&synchronization.Configuration{},
		false,
	)

	// Unreachable: Connect panics on non-graft URLs before returning.
	test.That(t, err, test.ShouldBeNil)
}

func TestSyncConnResolver(t *testing.T) {
	connMgr := NewConnectionManager(slog.LevelDebug)
	t.Cleanup(connMgr.Close)

	daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
	daemon.runCtx = connMgr.runCtx
	daemon.setState(ConnectionStateConnected)

	conn, err := connMgr.createConnection("c", t.TempDir(), "/remote", daemon, false)
	test.That(t, err, test.ShouldBeNil)

	resolver := syncConnResolver(connMgr)

	t.Run("unknown connection errors", func(t *testing.T) {
		_, err := resolver("missing")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "connection not found")
	})

	t.Run("connection without a live remote conn errors", func(t *testing.T) {
		_, err := resolver("c")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "not available")
	})

	t.Run("connection with a live remote conn resolves", func(t *testing.T) {
		cc, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
		test.That(t, err, test.ShouldBeNil)
		t.Cleanup(func() { cc.Close() })

		daemon.remoteConn = &fakeRemoteDaemonConn{cc: cc}

		got, err := resolver("c")
		test.That(t, err, test.ShouldBeNil)
		test.That(t, got, test.ShouldEqual, cc)
	})

	t.Run("resolves while the connection mutex is held", func(t *testing.T) {
		cc, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
		test.That(t, err, test.ShouldBeNil)
		t.Cleanup(func() { cc.Close() })

		daemon.remoteConn = &fakeRemoteDaemonConn{cc: cc}

		// The resolver runs inside mutagen's synchronous Create/Resume calls,
		// which EstablishSynchronization makes while holding conn.mu. It must
		// therefore never take conn.mu itself, or it self-deadlocks.
		conn.mu.Lock()
		defer conn.mu.Unlock()

		done := make(chan struct{})

		var got *grpc.ClientConn

		var resolveErr error

		go func() {
			defer close(done)

			got, resolveErr = resolver("c")
		}()

		select {
		case <-done:
			test.That(t, resolveErr, test.ShouldBeNil)
			test.That(t, got, test.ShouldEqual, cc)
		case <-time.After(10 * time.Second):
			test.That(t, "resolver deadlocked on conn.mu", test.ShouldBeEmpty)
		}
	})
}

func TestEstablishSynchronizationWithRealResolverDoesNotDeadlock(t *testing.T) {
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())

	syncMgr, err := synchronization.NewManagerWithoutPersistence(logging.NewLoggerOnSlogger(slog.Default()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(syncMgr.Shutdown)

	connMgr := NewConnectionManager(slog.LevelDebug)
	t.Cleanup(connMgr.Close)

	daemon := newRemoteDaemon(&echoConnector{}, slog.LevelDebug)
	daemon.runCtx = connMgr.runCtx
	daemon.setState(ConnectionStateConnected)

	cc, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { cc.Close() })

	daemon.remoteConn = &fakeRemoteDaemonConn{cc: cc}

	conn, err := connMgr.createConnection("test", t.TempDir(), "/remote", daemon, false)
	test.That(t, err, test.ShouldBeNil)

	// The REAL protocol handler with the REAL resolver (no stub).
	proto := urlpkg.Protocol(syncProtoNum)
	prev := synchronization.ProtocolHandlers[proto]
	synchronization.ProtocolHandlers[proto] = &mutagenSyncProtocolHandler{
		resolveConn: syncConnResolver(connMgr),
	}

	t.Cleanup(func() { synchronization.ProtocolHandlers[proto] = prev })

	// EstablishSynchronization holds conn.mu while syncManager.Create eagerly
	// connects beta through the resolver. The passthrough conn dials nowhere,
	// so Create fails fast at the beta handshake; the point is that it
	// RETURNS instead of self-deadlocking on conn.mu.
	done := make(chan error, 1)

	go func() {
		_, err := conn.EstablishSynchronization(t.Context(), SynchronizationIntent{
			FromLocal: ".",
			ToRemote:  "/remote/x",
		}, syncMgr)
		done <- err
	}()

	select {
	case err := <-done:
		test.That(t, err, test.ShouldNotBeNil)
	case <-time.After(30 * time.Second):
		test.That(t, "deadlock: EstablishSynchronization did not return", test.ShouldBeEmpty)
	}
}

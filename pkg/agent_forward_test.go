package graft

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// fakeAgentForwardServer implements the ForwardSSHAgent bidi stream server
// side: the first Recv yields the queued handshake message and later Recvs
// block until the context is canceled.
type fakeAgentForwardServer struct {
	grpc.ServerStream

	//nolint:containedctx // a stream's context is inherent to the faked interface
	ctx context.Context

	mu        sync.Mutex
	handshake *graftv1.ForwardSSHAgentRequest
}

func newFakeAgentForwardServer(ctx context.Context, connName string) *fakeAgentForwardServer {
	return &fakeAgentForwardServer{
		ctx:       ctx,
		handshake: &graftv1.ForwardSSHAgentRequest{ConnectionName: connName},
	}
}

func (f *fakeAgentForwardServer) Context() context.Context { return f.ctx }

func (f *fakeAgentForwardServer) Recv() (*graftv1.ForwardSSHAgentRequest, error) {
	f.mu.Lock()
	msg := f.handshake
	f.handshake = nil
	f.mu.Unlock()

	if msg != nil {
		return msg, nil
	}

	<-f.ctx.Done()

	return nil, errors.Wrap(f.ctx.Err())
}

func (f *fakeAgentForwardServer) Send(*graftv1.ForwardSSHAgentResponse) error { return nil }

// waitForAgentForwardEntry polls until srv's forward entry for connName
// satisfies want (e.g. registered, replaced, or gone) and returns it.
func waitForAgentForwardEntry(
	t *testing.T,
	srv *Server,
	connName string,
	want func(*sshAgentForward) bool,
) *sshAgentForward {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		srv.serverMu.Lock()
		entry := srv.sshAuthSockPaths[connName]
		srv.serverMu.Unlock()

		if want(entry) {
			return entry
		}

		time.Sleep(5 * time.Millisecond)
	}

	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	test.That(t, want(srv.sshAuthSockPaths[connName]), test.ShouldBeTrue)

	return srv.sshAuthSockPaths[connName]
}

func TestForwardSSHAgentReplacesStaleForward(t *testing.T) {
	srv := &Server{role: ServerRoleRemote}

	// First forward registers and parks in Accept.
	ctx1 := t.Context()

	err1 := make(chan error, 1)

	go func() { err1 <- srv.ForwardSSHAgent(newFakeAgentForwardServer(ctx1, "c")) }()

	first := waitForAgentForwardEntry(t, srv, "c", func(e *sshAgentForward) bool { return e != nil })

	// A second forward for the same connection arrives while the first is
	// still registered (its stream outlived the local side, so cleanup never
	// ran). The new stream is by definition the live one: it must replace the
	// stale entry, not be rejected. The old behavior - erroring with "already
	// active" - made the local side retry in a hot loop.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	err2 := make(chan error, 1)

	go func() { err2 <- srv.ForwardSSHAgent(newFakeAgentForwardServer(ctx2, "c")) }()

	second := waitForAgentForwardEntry(t, srv, "c", func(e *sshAgentForward) bool {
		return e != nil && e.sockPath != first.sockPath
	})
	test.That(t, second, test.ShouldNotEqual, first)

	// Exactly one entry exists, and it's the replacement's.
	srv.serverMu.Lock()
	test.That(t, len(srv.sshAuthSockPaths), test.ShouldEqual, 1)
	test.That(t, srv.sshAuthSockPaths["c"], test.ShouldEqual, second)
	srv.serverMu.Unlock()

	// The replaced handler unwound (its listener was closed) and did not
	// clobber the replacement's map entry or socket file.
	select {
	case <-err1:
	case <-time.After(5 * time.Second):
		test.That(t, "timeout waiting for replaced handler to unwind", test.ShouldBeEmpty)
	}

	_, statErr := os.Stat(first.sockPath)
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)

	srv.serverMu.Lock()
	test.That(t, srv.sshAuthSockPaths["c"], test.ShouldEqual, second)
	srv.serverMu.Unlock()

	// Canceling the replacement's stream ends its handler and cleans up its
	// own entry.
	cancel2()
	test.That(t, <-err2, test.ShouldBeNil)
	waitForAgentForwardEntry(t, srv, "c", func(e *sshAgentForward) bool { return e == nil })
}

func TestHoldAgentForwardBackoffSkipsCanceledContext(t *testing.T) {
	// An explicit stop racing the failure cancels the forward's context
	// first; recording a failure behind it would leave a stale count that
	// doubles the next start's backoff.
	daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	daemon.holdAgentForwardBackoff(ctx, "c")

	daemon.mu.Lock()
	test.That(t, daemon.agentForwardFailures, test.ShouldBeEmpty)
	daemon.mu.Unlock()
}

func TestAgentForwardBackoffDuration(t *testing.T) {
	oldBase, oldMax := agentForwardBaseBackoff, agentForwardMaxBackoff
	agentForwardBaseBackoff = time.Second
	agentForwardMaxBackoff = 30 * time.Second

	t.Cleanup(func() { agentForwardBaseBackoff, agentForwardMaxBackoff = oldBase, oldMax })

	test.That(t, agentForwardBackoffFor(1), test.ShouldEqual, time.Second)
	test.That(t, agentForwardBackoffFor(2), test.ShouldEqual, 2*time.Second)
	test.That(t, agentForwardBackoffFor(3), test.ShouldEqual, 4*time.Second)
	test.That(t, agentForwardBackoffFor(6), test.ShouldEqual, 30*time.Second)
	// Many consecutive failures stay capped, never overflowing the shift.
	test.That(t, agentForwardBackoffFor(64), test.ShouldEqual, 30*time.Second)
}

func TestStartAgentForwardBackoff(t *testing.T) {
	oldBase := agentForwardBaseBackoff
	agentForwardBaseBackoff = time.Hour // only an explicit cancel unblocks the test

	t.Cleanup(func() { agentForwardBaseBackoff = oldBase })

	daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemon.runCtx = ctx

	daemon.startAgentForward("c")

	// The forward fails immediately (no remote client conn). Once the failure
	// is recorded, the slot stays held for the backoff window, so the
	// reconcile loop's every-second startAgentForward calls no-op instead of
	// hot-looping restarts (~120 errors/min in production).
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		daemon.mu.Lock()
		failures := daemon.agentForwardFailures["c"]
		daemon.mu.Unlock()

		if failures == 1 {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	daemon.mu.Lock()
	test.That(t, daemon.agentForwardFailures["c"], test.ShouldEqual, 1)
	daemon.mu.Unlock()

	_, _, started := daemon.tryBeginAgentForward("c")
	test.That(t, started, test.ShouldBeFalse)

	// Tearing down the daemon context unblocks the backoff wait and frees
	// the slot.
	cancel()

	deadline = time.Now().Add(5 * time.Second)

	var freed bool

	for time.Now().Before(deadline) {
		if _, _, started := daemon.tryBeginAgentForward("c"); started {
			freed = true

			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	test.That(t, freed, test.ShouldBeTrue)

	// Clean up the slot the successful probe reserved.
	daemon.endAgentForward("c")
}

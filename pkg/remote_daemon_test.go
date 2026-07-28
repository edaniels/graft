package graft

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"go.viam.com/test"

	"github.com/edaniels/graft/errors"
)

const (
	fakeLinuxOS   = "Linux"
	fakeAMD64Arch = "x86_64"
	fakeHomeDir   = "/home/user"
)

// combinedDiscoverCmd is the single command used by discoverRemote.
var combinedDiscoverCmd = unameOSCmd + " && " + unameArchCmd + " && " + homeDirCmd

type fakeDiscoverer struct {
	noopConnector

	runOneShotFunc func(ctx context.Context, command string) (string, error)
	identity       string
}

func (f *fakeDiscoverer) RunOneShotCommand(ctx context.Context, command string) (string, error) {
	return f.runOneShotFunc(ctx, command)
}

func (f *fakeDiscoverer) Identity() string {
	return f.identity
}

func TestRemoteDaemonDiscover(t *testing.T) {
	t.Run("caches results across calls", func(t *testing.T) {
		callCount := 0
		connector := &fakeDiscoverer{
			runOneShotFunc: func(_ context.Context, cmd string) (string, error) {
				callCount++

				switch cmd {
				case combinedDiscoverCmd:
					return fakeLinuxOS + "\n" + fakeAMD64Arch + "\n" + fakeHomeDir + "\n", nil
				default:
					return "", nil
				}
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)
		ctx := context.Background()

		// First call discovers.
		info, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info.OS, test.ShouldEqual, "linux")
		test.That(t, info.Arch, test.ShouldEqual, "amd64")
		test.That(t, info.HomeDir, test.ShouldEqual, "/home/user")
		test.That(t, info.RemoteSocketPath, test.ShouldNotBeBlank)
		test.That(t, callCount, test.ShouldEqual, 1) // single combined command

		// Second call returns cached results without running commands.
		info2, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info2.OS, test.ShouldEqual, "linux")
		test.That(t, callCount, test.ShouldEqual, 1) // no new calls
	})

	t.Run("cached result shared by daemon", func(t *testing.T) {
		callCount := 0
		connector := &fakeDiscoverer{
			runOneShotFunc: func(_ context.Context, cmd string) (string, error) {
				callCount++

				switch cmd {
				case combinedDiscoverCmd:
					return "Darwin\narm64\n/Users/test\n", nil
				default:
					return "", nil
				}
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)
		ctx := context.Background()

		info1, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info1.OS, test.ShouldEqual, "darwin")
		test.That(t, info1.Arch, test.ShouldEqual, "arm64")
		test.That(t, callCount, test.ShouldEqual, 1)

		// Second call gets cached result.
		info2, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info2, test.ShouldResemble, info1)
		test.That(t, callCount, test.ShouldEqual, 1) // still 1
	})

	t.Run("with identity includes identity in socket path", func(t *testing.T) {
		connector := &fakeDiscoverer{
			identity: "bright-falcon-soar",
			runOneShotFunc: func(_ context.Context, cmd string) (string, error) {
				switch cmd {
				case combinedDiscoverCmd:
					return fakeLinuxOS + "\n" + fakeAMD64Arch + "\n" + fakeHomeDir + "\n", nil
				default:
					return "", nil
				}
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)
		ctx := context.Background()

		info, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, info.RemoteSocketPath, test.ShouldContainSubstring, "bright-falcon-soar")
	})

	t.Run("concurrent calls only discover once", func(t *testing.T) {
		var (
			callCount int
			mu        sync.Mutex
		)

		connector := &fakeDiscoverer{
			runOneShotFunc: func(_ context.Context, cmd string) (string, error) {
				mu.Lock()

				callCount++

				mu.Unlock()

				switch cmd {
				case combinedDiscoverCmd:
					return fakeLinuxOS + "\n" + fakeAMD64Arch + "\n" + fakeHomeDir + "\n", nil
				default:
					return "", nil
				}
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)
		ctx := context.Background()

		var wg sync.WaitGroup

		const numGoroutines = 10

		for range numGoroutines {
			wg.Go(func() {
				info, err := daemon.discover(ctx)
				test.That(t, err, test.ShouldBeNil)
				test.That(t, info.OS, test.ShouldEqual, "linux")
			})
		}

		wg.Wait()
		test.That(t, callCount, test.ShouldEqual, 1)
	})

	t.Run("resetDiscovery allows re-discovery", func(t *testing.T) {
		callCount := 0
		connector := &fakeDiscoverer{
			runOneShotFunc: func(_ context.Context, cmd string) (string, error) {
				callCount++

				switch cmd {
				case combinedDiscoverCmd:
					return fakeLinuxOS + "\n" + fakeAMD64Arch + "\n" + fakeHomeDir + "\n", nil
				default:
					return "", nil
				}
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)
		ctx := context.Background()

		_, err := daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, callCount, test.ShouldEqual, 1)

		daemon.resetDiscovery()

		_, err = daemon.discover(ctx)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, callCount, test.ShouldEqual, 2) // re-discovered
	})
}

type fakeInitConnector struct {
	noopConnector

	initFunc func(ctx context.Context) (bool, error)
}

func (f *fakeInitConnector) InitializeRemote(ctx context.Context) (bool, error) {
	if f.initFunc != nil {
		return f.initFunc(ctx)
	}

	return false, nil
}

func TestRemoteDaemonInitialize(t *testing.T) {
	t.Run("transport failure sets Failed state", func(t *testing.T) {
		connector := &fakeInitConnector{
			initFunc: func(_ context.Context) (bool, error) {
				return false, errors.New("transport failed")
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)

		err := daemon.Initialize(context.Background(), nil)
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "transport failed")

		state, _ := daemon.State()
		test.That(t, state, test.ShouldEqual, ConnectionStateFailed)
	})

	t.Run("afterTransport supersede aborts initialization", func(t *testing.T) {
		connector := &fakeInitConnector{
			initFunc: func(_ context.Context) (bool, error) {
				return false, nil
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)

		err := daemon.Initialize(context.Background(), func() error {
			return errDaemonSuperseded
		})
		test.That(t, errors.Is(err, errDaemonSuperseded), test.ShouldBeTrue)
	})

	t.Run("connected daemon is no-op", func(t *testing.T) {
		daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		daemon.state = ConnectionStateConnected

		err := daemon.Initialize(context.Background(), nil)
		test.That(t, err, test.ShouldBeNil)
	})

	t.Run("invalid state returns error", func(t *testing.T) {
		for _, state := range []ConnectionState{
			ConnectionStateFailed,
			ConnectionStateClosed,
			ConnectionStateReconnecting,
		} {
			daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
			daemon.state = state

			err := daemon.Initialize(context.Background(), nil)
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, "cannot Initialize")
		}
	})

	t.Run("concurrent callers only initialize transport once", func(t *testing.T) {
		var initCount atomic.Int32

		connector := &fakeInitConnector{
			initFunc: func(_ context.Context) (bool, error) {
				initCount.Add(1)

				return false, errors.New("transport failed")
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)

		var wg sync.WaitGroup

		const numGoroutines = 10

		for range numGoroutines {
			wg.Go(func() {
				err := daemon.Initialize(context.Background(), nil)
				test.That(t, err, test.ShouldNotBeNil)
				test.That(t, err.Error(), test.ShouldContainSubstring, "transport failed")
			})
		}

		wg.Wait()
		test.That(t, initCount.Load(), test.ShouldEqual, 1)
	})

	t.Run("blocked caller respects context cancellation", func(t *testing.T) {
		transportStarted := make(chan struct{})
		transportContinue := make(chan struct{})

		connector := &fakeInitConnector{
			initFunc: func(_ context.Context) (bool, error) {
				close(transportStarted)
				<-transportContinue

				return false, errors.New("transport failed")
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)

		// First caller: blocks in InitializeRemote.
		go func() {
			daemon.Initialize(context.Background(), nil) //nolint:errcheck
		}()

		<-transportStarted

		// Second caller with cancellable context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := daemon.Initialize(ctx, nil)
		test.That(t, err, test.ShouldNotBeNil)

		close(transportContinue)
	})

	t.Run("concurrent callers all see supersede result", func(t *testing.T) {
		transportStarted := make(chan struct{})
		transportContinue := make(chan struct{})

		connector := &fakeInitConnector{
			initFunc: func(_ context.Context) (bool, error) {
				close(transportStarted)
				<-transportContinue

				return false, nil
			},
		}

		daemon := newRemoteDaemon(connector, slog.LevelDebug)

		errCh := make(chan error, 2)

		// First caller: will trigger supersede in afterTransport.
		go func() {
			errCh <- daemon.Initialize(context.Background(), func() error {
				return errDaemonSuperseded
			})
		}()

		<-transportStarted

		// Second caller: blocks on initDone, sees supersede result.
		go func() {
			errCh <- daemon.Initialize(context.Background(), nil)
		}()

		close(transportContinue)

		err1 := <-errCh
		err2 := <-errCh

		test.That(t, errors.Is(err1, errDaemonSuperseded), test.ShouldBeTrue)
		test.That(t, errors.Is(err2, errDaemonSuperseded), test.ShouldBeTrue)
	})
}

func TestRemoteDaemonInstallGuard(t *testing.T) {
	t.Run("markInstalled prevents second install", func(t *testing.T) {
		daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		test.That(t, daemon.alreadyInstalled(), test.ShouldBeFalse)

		daemon.markInstalled()
		test.That(t, daemon.alreadyInstalled(), test.ShouldBeTrue)
	})

	t.Run("resetInstallState allows reinstall", func(t *testing.T) {
		daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		daemon.markInstalled()
		test.That(t, daemon.alreadyInstalled(), test.ShouldBeTrue)

		daemon.resetInstallState()
		test.That(t, daemon.alreadyInstalled(), test.ShouldBeFalse)
	})

	t.Run("shared across connections", func(t *testing.T) {
		daemon := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)

		conn1 := newConnection(daemon, "conn1", "", "", false)
		conn2 := newConnection(daemon, "conn2", "", "", false)

		test.That(t, conn1.daemon.alreadyInstalled(), test.ShouldBeFalse)
		test.That(t, conn2.daemon.alreadyInstalled(), test.ShouldBeFalse)

		// conn1 installs
		conn1.daemon.markInstalled()

		// conn2 sees it
		test.That(t, conn2.daemon.alreadyInstalled(), test.ShouldBeTrue)

		// Daemon goes down; reset
		conn1.daemon.resetInstallState()
		test.That(t, conn2.daemon.alreadyInstalled(), test.ShouldBeFalse)
	})
}

func TestRemoteDaemonAgentForwardGuard(t *testing.T) {
	t.Run("second concurrent begin for the same connection is rejected until the first ends", func(t *testing.T) {
		d := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		d.runCtx = context.Background()

		ctx1, _, started1 := d.tryBeginAgentForward("conn-a")
		test.That(t, started1, test.ShouldBeTrue)
		test.That(t, ctx1, test.ShouldNotBeNil)

		_, _, started2 := d.tryBeginAgentForward("conn-a")
		test.That(t, started2, test.ShouldBeFalse)
		test.That(t, ctx1.Err(), test.ShouldBeNil)

		d.endAgentForward("conn-a")
		test.That(t, ctx1.Err(), test.ShouldNotBeNil)

		ctx3, _, started3 := d.tryBeginAgentForward("conn-a")
		test.That(t, started3, test.ShouldBeTrue)
		test.That(t, ctx3, test.ShouldNotBeNil)
		test.That(t, ctx3.Err(), test.ShouldBeNil)
	})

	t.Run("endAgentForward is a no-op when nothing is forwarding", func(t *testing.T) {
		d := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		d.runCtx = context.Background()

		d.endAgentForward("conn-a")
		d.stopAgentForward("conn-a")

		_, _, started := d.tryBeginAgentForward("conn-a")
		test.That(t, started, test.ShouldBeTrue)
	})

	t.Run("two different connections forward independently on a shared daemon", func(t *testing.T) {
		d := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		d.runCtx = context.Background()

		ctxA, _, startedA := d.tryBeginAgentForward("conn-a")
		test.That(t, startedA, test.ShouldBeTrue)

		ctxB, _, startedB := d.tryBeginAgentForward("conn-b")
		test.That(t, startedB, test.ShouldBeTrue)

		// Stopping one must not affect the other.
		d.stopAgentForward("conn-a")
		test.That(t, ctxA.Err(), test.ShouldNotBeNil)
		test.That(t, ctxB.Err(), test.ShouldBeNil)

		// conn-a can be restarted independently of conn-b still running.
		ctxA2, _, startedA2 := d.tryBeginAgentForward("conn-a")
		test.That(t, startedA2, test.ShouldBeTrue)
		test.That(t, ctxA2.Err(), test.ShouldBeNil)
		test.That(t, ctxB.Err(), test.ShouldBeNil)
	})

	t.Run("tryBeginAgentForward refuses to start once the daemon is closed", func(t *testing.T) {
		d := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		d.runCtx = context.Background()
		d.closed = true

		_, _, started := d.tryBeginAgentForward("conn-a")
		test.That(t, started, test.ShouldBeFalse)
	})

	t.Run("a stale worker cannot cancel a newer forward for the same connection (ABA)", func(t *testing.T) {
		d := newRemoteDaemon(&noopConnector{}, slog.LevelDebug)
		d.runCtx = context.Background()

		// Simulate the first forward's worker: begin, then it gets stopped
		// out from under it (e.g. a reconnect or an explicit stop) before
		// its own cleanup runs.
		_, gen1, started1 := d.tryBeginAgentForward("conn-a")
		test.That(t, started1, test.ShouldBeTrue)

		d.stopAgentForward("conn-a")

		// A newer forward is registered for the same connection name.
		ctx2, gen2, started2 := d.tryBeginAgentForward("conn-a")
		test.That(t, started2, test.ShouldBeTrue)
		test.That(t, gen2, test.ShouldNotEqual, gen1)

		// The first (stale) worker's cleanup call, keyed by its own
		// generation, must not tear down the second forward.
		d.endAgentForwardGen("conn-a", gen1)
		test.That(t, ctx2.Err(), test.ShouldBeNil)

		_, _, startedAgain := d.tryBeginAgentForward("conn-a")
		test.That(t, startedAgain, test.ShouldBeFalse) // conn-a is still gen2's forward

		// The second (current) worker's cleanup, with the right generation,
		// does tear it down.
		d.endAgentForwardGen("conn-a", gen2)
		test.That(t, ctx2.Err(), test.ShouldNotBeNil)
	})
}

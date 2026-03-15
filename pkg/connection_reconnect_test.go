package graft

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
)

// mockRemoteDaemonConnection implements RemoteDaemonConnection for testing.
type mockRemoteDaemonConnection struct {
	clientConn *grpc.ClientConn
}

func newMockRemoteDaemonConnection() *mockRemoteDaemonConnection {
	var lc net.ListenConfig

	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		lis.Close()
		panic(err)
	}

	lis.Close()

	return &mockRemoteDaemonConnection{clientConn: cc}
}

func (m *mockRemoteDaemonConnection) ClientConn() *grpc.ClientConn {
	return m.clientConn
}

func (m *mockRemoteDaemonConnection) Close() error {
	if err := m.clientConn.Close(); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

// mockReconnectConnector implements RemoteConnector for reconnect tests.
type mockReconnectConnector struct {
	destination string
	initCount   atomic.Int32
	closeCount  atomic.Int32
	mu          sync.Mutex
	initErr     error
	connectOK   bool
	oneShotOut  string
	initGate    chan struct{} // if non-nil, InitializeRemote blocks until closed
	onInit      func(count int32)
}

func (m *mockReconnectConnector) Destination() string     { return m.destination }
func (m *mockReconnectConnector) SafeDestination() string { return m.destination }
func (m *mockReconnectConnector) Identity() string        { return "" }
func (m *mockReconnectConnector) StateFields() []any      { return nil }

func (m *mockReconnectConnector) InitializeRemote(ctx context.Context) (bool, error) {
	count := m.initCount.Add(1)

	if m.onInit != nil {
		m.onInit(count)
	}

	if m.initGate != nil {
		select {
		case <-m.initGate:
		case <-ctx.Done():
			return false, errors.Wrap(context.Cause(ctx))
		}
	}

	m.mu.Lock()
	err := m.initErr
	m.mu.Unlock()

	if err != nil {
		return false, err
	}

	return false, nil
}

func (m *mockReconnectConnector) DeinitializeRemote(_ context.Context) error {
	return nil
}

func (m *mockReconnectConnector) RunOneShotCommand(_ context.Context, command string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch command {
	case unameOSCmd + " && " + unameArchCmd + " && " + homeDirCmd:
		return "Linux\nx86_64\n" + m.oneShotOut + "\n", nil
	default:
		return "", nil
	}
}

func (m *mockReconnectConnector) ConnectToRemoteDaemon(
	_ context.Context,
	_ string,
	_ string,
) (RemoteDaemonConnection, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.connectOK {
		return nil, false, nil
	}

	return newMockRemoteDaemonConnection(), true, nil
}

func (m *mockReconnectConnector) CopyFile(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockReconnectConnector) Close() error {
	m.closeCount.Add(1)

	return nil
}

func newTestDaemon(t *testing.T, connector *mockReconnectConnector) *remoteDaemon {
	t.Helper()

	d := newRemoteDaemon(connector)
	d.runCtx = t.Context()
	d.mu.Lock()
	d.remoteConn = newMockRemoteDaemonConnection()
	d.state = ConnectionStateConnected
	d.homeDir = "/home/test"
	d.mu.Unlock()

	return d
}

func TestReconnectSkipsClosedConnection(t *testing.T) {
	connector := &mockReconnectConnector{destination: "test://host"}
	daemon := newTestDaemon(t, connector)

	test.That(t, daemon.Close(), test.ShouldBeNil)

	result := daemon.Reconnect(t.Context())
	test.That(t, result, test.ShouldBeFalse)

	state, _ := daemon.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateClosed)
}

func TestReconnectConcurrentAttemptsPrevented(t *testing.T) {
	connector := &mockReconnectConnector{destination: "test://host"}
	daemon := newTestDaemon(t, connector)

	daemon.mu.Lock()
	daemon.reconnecting = true
	daemon.mu.Unlock()

	result := daemon.Reconnect(t.Context())
	test.That(t, result, test.ShouldBeFalse)
}

func TestReconnectRetriesUntilSuccess(t *testing.T) {
	connector := &mockReconnectConnector{
		destination: "test://host",
		initErr:     errConnectionNotFound,
	}

	// After 2 failed init attempts, fix the connector so the 3rd succeeds.
	connector.onInit = func(count int32) {
		if count == 3 {
			connector.mu.Lock()
			connector.initErr = nil
			connector.connectOK = true
			connector.oneShotOut = "/home/test"
			connector.mu.Unlock()
		}
	}

	daemon := newTestDaemon(t, connector)

	result := daemon.Reconnect(t.Context())
	test.That(t, result, test.ShouldBeTrue)
	test.That(t, connector.initCount.Load(), test.ShouldBeGreaterThanOrEqualTo, int32(3))

	state, _ := daemon.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)
}

func TestReconnectAbortsOnClose(t *testing.T) {
	connector := &mockReconnectConnector{
		destination: "test://host",
		initErr:     errConnectionNotFound, // always fail so it keeps retrying
	}
	daemon := newTestDaemon(t, connector)

	// Close the daemon after a few attempts to break the retry loop.
	connector.onInit = func(count int32) {
		if count == 3 {
			daemon.mu.Lock()
			daemon.closed = true
			daemon.mu.Unlock()
		}
	}

	result := daemon.Reconnect(t.Context())
	test.That(t, result, test.ShouldBeFalse)
}

func TestReconnectTransportReInit(t *testing.T) {
	connector := &mockReconnectConnector{
		destination: "test://host",
		connectOK:   true,
		oneShotOut:  "/home/test",
	}
	daemon := newTestDaemon(t, connector)

	initBefore := connector.initCount.Load()
	closeBefore := connector.closeCount.Load()

	result := daemon.Reconnect(t.Context())
	test.That(t, result, test.ShouldBeTrue)

	test.That(t, connector.closeCount.Load(), test.ShouldEqual, closeBefore+1)
	test.That(t, connector.initCount.Load(), test.ShouldEqual, initBefore+1)

	state, _ := daemon.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)
}

func TestReconnectStateTransitions(t *testing.T) {
	// Block InitializeRemote until we've observed the Reconnecting state.
	initGate := make(chan struct{})
	connector := &mockReconnectConnector{
		destination: "test://host",
		connectOK:   true,
		oneShotOut:  "/home/test",
		initGate:    initGate,
	}
	daemon := newTestDaemon(t, connector)

	state, _ := daemon.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)

	done := make(chan bool, 1)

	go func() {
		done <- daemon.Reconnect(t.Context())
	}()

	// Wait until state transitions to Reconnecting.
	sawReconnecting := make(chan struct{})

	go func() {
		for {
			st, _ := daemon.State()
			if st == ConnectionStateReconnecting {
				close(sawReconnecting)

				return
			}
		}
	}()

	select {
	case <-sawReconnecting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Reconnecting state")
	}

	// Unblock InitializeRemote so reconnect can complete.
	close(initGate)

	select {
	case result := <-done:
		test.That(t, result, test.ShouldBeTrue)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for Reconnect to complete")
	}

	state, _ = daemon.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)
}

func TestConnectionStateDerivedFromDaemon(t *testing.T) {
	connector := &mockReconnectConnector{destination: "test://host"}
	daemon := newTestDaemon(t, connector)

	conn := newConnection(daemon, "myconn", "/local", "/remote")

	// Connection derives Connected from daemon.
	state, _ := conn.State()
	test.That(t, state, test.ShouldEqual, ConnectionStateConnected)

	// Close connection — it reports Closed while daemon is still Connected.
	test.That(t, conn.Close(), test.ShouldBeNil)

	connState, _ := conn.State()
	test.That(t, connState, test.ShouldEqual, ConnectionStateClosed)

	daemonState, _ := daemon.State()
	test.That(t, daemonState, test.ShouldEqual, ConnectionStateConnected)
}

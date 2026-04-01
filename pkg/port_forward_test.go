package graft

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

const noopConnectorName = "test"

func TestPortConflictDetectionTCP(t *testing.T) {
	// Occupy a TCP port.
	var lc net.ListenConfig

	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	defer listener.Close()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	test.That(t, ok, test.ShouldBeTrue)

	port := uint32(tcpAddr.Port) //nolint:gosec // test code

	// probePortConflict should detect the occupied port.
	conflict, reason := probePortConflict("tcp", port)
	test.That(t, conflict, test.ShouldBeTrue)
	test.That(t, reason, test.ShouldContainSubstring, "already in use")

	// startPortForward should mark it as conflicted.
	daemon := newRemoteDaemon(&noopConnector{})
	fwd := daemon.startPortForward(t.Context(), &graftv1.PortInfo{
		Port:     port,
		Host:     "127.0.0.1",
		Protocol: "tcp",
	})
	test.That(t, fwd.conflict, test.ShouldBeTrue)
	test.That(t, fwd.conflictReason, test.ShouldContainSubstring, "already in use")

	// An unoccupied port should not conflict.
	conflict, _ = probePortConflict("tcp", 0)
	test.That(t, conflict, test.ShouldBeFalse)
}

func TestPortConflictDetectionUDP(t *testing.T) {
	// Occupy a UDP port.
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	udpConn, err := net.ListenUDP("udp", udpAddr)
	test.That(t, err, test.ShouldBeNil)

	defer udpConn.Close()

	localAddr, ok := udpConn.LocalAddr().(*net.UDPAddr)
	test.That(t, ok, test.ShouldBeTrue)

	port := uint32(localAddr.Port) //nolint:gosec // test code

	// probePortConflict should detect the occupied port.
	conflict, reason := probePortConflict("udp", port)
	test.That(t, conflict, test.ShouldBeTrue)
	test.That(t, reason, test.ShouldContainSubstring, "already in use")

	// startPortForward should mark it as conflicted.
	daemon := newRemoteDaemon(&noopConnector{})
	fwd := daemon.startPortForward(t.Context(), &graftv1.PortInfo{
		Port:     port,
		Host:     "127.0.0.1",
		Protocol: "udp",
	})
	test.That(t, fwd.conflict, test.ShouldBeTrue)
	test.That(t, fwd.conflictReason, test.ShouldContainSubstring, "already in use")
}

func TestListeningPortsEqual(t *testing.T) {
	a := []ListeningPort{
		{Port: 8080, Host: "0.0.0.0", Protocol: "tcp"},
		{Port: 3000, Host: "0.0.0.0", Protocol: "tcp"},
	}
	b := []ListeningPort{
		{Port: 8080, Host: "0.0.0.0", Protocol: "tcp"},
		{Port: 3000, Host: "0.0.0.0", Protocol: "tcp"},
	}

	test.That(t, listeningPortsEqual(a, b), test.ShouldBeTrue)

	c := []ListeningPort{
		{Port: 8080, Host: "0.0.0.0", Protocol: "tcp"},
	}

	test.That(t, listeningPortsEqual(a, c), test.ShouldBeFalse)

	d := []ListeningPort{
		{Port: 8080, Host: "0.0.0.0", Protocol: "tcp"},
		{Port: 3000, Host: "0.0.0.0", Protocol: "udp"},
	}

	test.That(t, listeningPortsEqual(a, d), test.ShouldBeFalse)
}

// --- Test infrastructure for gRPC-based port forwarding tests ---

// testRemoteDaemonConn wraps a *grpc.ClientConn to satisfy RemoteDaemonConnection.
type testRemoteDaemonConn struct {
	cc *grpc.ClientConn
}

func (c *testRemoteDaemonConn) ClientConn() *grpc.ClientConn { return c.cc }
func (c *testRemoteDaemonConn) Close() error                 { return errors.Wrap(c.cc.Close()) }

// startTestRemoteDaemon starts a real Server with ServerRoleRemote and returns its socket path.
func startTestRemoteDaemon(t *testing.T) string {
	t.Helper()

	// Use a short temp dir path to stay within macOS's 104-byte Unix socket path limit.
	tmpDir, err := os.MkdirTemp("/tmp", "st-") //nolint:usetesting // t.TempDir path too long for Unix socket
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	t.Setenv("GRAFT_STATE_HOME", tmpDir)

	srv, err := NewServer(&RootConfig{}, ServerRoleRemote, "", true, &BufferedLineWriter{MaxLines: 10}, "")
	test.That(t, err, test.ShouldBeNil)

	test.That(t, srv.Run(t.Context()), test.ShouldBeNil)

	t.Cleanup(srv.Close)

	sockPath, err := DaemonSocketPathForCurrentHost(ServerRoleRemote)
	test.That(t, err, test.ShouldBeNil)

	return sockPath
}

// connectToTestDaemon creates a gRPC client connection to the daemon at sockPath.
func connectToTestDaemon(t *testing.T, sockPath string) RemoteDaemonConnection {
	t.Helper()

	cc, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() { cc.Close() })

	return &testRemoteDaemonConn{cc: cc}
}

// startEchoServer starts a TCP echo server (io.Copy) on a random port and returns the port.
func startEchoServer(t *testing.T) uint32 {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				defer c.Close()
				//nolint:errcheck // test echo server
				io.Copy(c, c)
			}()
		}
	}()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	test.That(t, ok, test.ShouldBeTrue)

	return uint32(tcpAddr.Port) //nolint:gosec // test code
}

// testDaemonForRelay returns a remoteDaemon with remoteConn set for relay testing.
func testDaemonForRelay(remoteConn RemoteDaemonConnection) *remoteDaemon {
	d := newRemoteDaemon(&noopConnector{})
	d.mu.Lock()
	d.remoteConn = remoteConn
	d.state = ConnectionStateConnected
	d.mu.Unlock()

	return d
}

// waitFor polls fn until it returns true or the test context expires.
func waitFor(t *testing.T, fn func() bool) error {
	t.Helper()

	for {
		if fn() {
			return nil
		}

		select {
		case <-t.Context().Done():
			return errors.Wrap(t.Context().Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestForwardPortTCPRelay tests the ForwardPort RPC + relayBidi end-to-end through
// a real remote daemon by sending data to an echo server and verifying it comes back.
func TestForwardPortTCPRelay(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	echoPort := startEchoServer(t)

	remoteConn := connectToTestDaemon(t, sockPath)
	client := graftv1.NewGraftServiceClient(remoteConn.ClientConn())

	stream, err := client.ForwardPort(t.Context())
	test.That(t, err, test.ShouldBeNil)

	err = stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Start{
			Start: &graftv1.ForwardPortStart{
				Port:     echoPort,
				Host:     "127.0.0.1",
				Protocol: "tcp",
			},
		},
	})
	test.That(t, err, test.ShouldBeNil)

	err = stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Payload{
			Payload: []byte("hello"),
		},
	})
	test.That(t, err, test.ShouldBeNil)

	resp, err := stream.Recv()
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(resp.GetPayload()), test.ShouldEqual, "hello")

	// Close send side, verify stream ends cleanly with EOF.
	err = stream.CloseSend()
	test.That(t, err, test.ShouldBeNil)

	_, err = stream.Recv()
	test.That(t, err, test.ShouldEqual, io.EOF)
}

// TestForwardPortTCPHalfClose tests TCP half-close propagation through relayBidi
// by using a server that reads until EOF before responding.
func TestForwardPortTCPHalfClose(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)

	// Start a "read-all-then-respond" server: reads until EOF, writes response, closes.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				defer c.Close()

				data, readErr := io.ReadAll(c)
				if readErr != nil {
					return
				}
				//nolint:errcheck // test server
				c.Write([]byte("got:" + string(data)))
			}()
		}
	}()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	test.That(t, ok, test.ShouldBeTrue)

	serverPort := uint32(tcpAddr.Port) //nolint:gosec // test code

	remoteConn := connectToTestDaemon(t, sockPath)
	client := graftv1.NewGraftServiceClient(remoteConn.ClientConn())

	stream, err := client.ForwardPort(t.Context())
	test.That(t, err, test.ShouldBeNil)

	err = stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Start{
			Start: &graftv1.ForwardPortStart{
				Port:     serverPort,
				Host:     "127.0.0.1",
				Protocol: "tcp",
			},
		},
	})
	test.That(t, err, test.ShouldBeNil)

	err = stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Payload{
			Payload: []byte("ping"),
		},
	})
	test.That(t, err, test.ShouldBeNil)

	// Close send side; propagates FIN to TCP server via CloseWrite.
	err = stream.CloseSend()
	test.That(t, err, test.ShouldBeNil)

	// The server only writes after seeing EOF, proving half-close propagated.
	var received []byte

	for {
		resp, recvErr := stream.Recv()
		if recvErr != nil {
			test.That(t, recvErr, test.ShouldEqual, io.EOF)

			break
		}

		received = append(received, resp.GetPayload()...)
	}

	test.That(t, string(received), test.ShouldEqual, "got:ping")
}

// TestHandleTCPForwardEndToEnd tests handleTCPForward end-to-end: listener accept ->
// relayTCPConnection -> gRPC -> ForwardPort -> echo -> back.
//
// We call handleTCPForward directly instead of startPortForward because startPortForward
// binds locally on the same port it dials on the remote. With both daemons on the same
// host, the echo server already holds that port so probePortConflict always flags it.
// The conflict path of startPortForward is covered by TestPortConflictDetection{TCP,UDP}.
func TestHandleTCPForwardEndToEnd(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	echoPort := startEchoServer(t)

	remoteConn := connectToTestDaemon(t, sockPath)
	daemon := testDaemonForRelay(remoteConn)

	var lc net.ListenConfig

	fwdLn, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	defer fwdLn.Close()

	go daemon.handleTCPForward(t.Context(), t.Context(), fwdLn, "127.0.0.1", echoPort)

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", fwdLn.Addr().String())
	test.That(t, err, test.ShouldBeNil)

	defer client.Close()

	_, err = client.Write([]byte("hello"))
	test.That(t, err, test.ShouldBeNil)

	buf := make([]byte, 5)

	_, err = io.ReadFull(client, buf)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(buf), test.ShouldEqual, "hello")
}

// TestRelaySurvivesListenerCancel verifies that when a port forward's listener
// is canceled (e.g. because the remote port left LISTEN state), existing TCP
// connections that were already accepted and relaying continue to work through
// the real gRPC path.
func TestRelaySurvivesListenerCancel(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	echoPort := startEchoServer(t)

	remoteConn := connectToTestDaemon(t, sockPath)
	daemon := testDaemonForRelay(remoteConn)

	// relayCtx outlives the forward; fwdCtx is canceled when the port leaves LISTEN state.
	relayCtx, relayCancel := context.WithCancel(t.Context())
	defer relayCancel()

	fwdCtx, fwdCancel := context.WithCancel(relayCtx)

	var lc net.ListenConfig

	fwdLn, err := lc.Listen(fwdCtx, "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	fwdAddr := fwdLn.Addr().String()

	go daemon.handleTCPForward(relayCtx, fwdCtx, fwdLn, "127.0.0.1", echoPort)

	// Connect a client and verify echo works.
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", fwdAddr)
	test.That(t, err, test.ShouldBeNil)

	defer client.Close()

	_, err = client.Write([]byte("hello"))
	test.That(t, err, test.ShouldBeNil)

	buf := make([]byte, 5)

	_, err = io.ReadFull(client, buf)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(buf), test.ShouldEqual, "hello")

	// Cancel fwdCtx; simulates port leaving LISTEN state.
	fwdCancel()

	// Poll until new connections are refused (listener closed).
	dialer := net.Dialer{Timeout: 50 * time.Millisecond}

	test.That(t, waitFor(t, func() bool {
		c, dialErr := dialer.DialContext(t.Context(), "tcp", fwdAddr)
		if dialErr != nil {
			return true
		}

		c.Close()

		return false
	}), test.ShouldBeNil)

	// Existing connection should STILL work; relay on relayCtx survives.
	_, err = client.Write([]byte("world"))
	test.That(t, err, test.ShouldBeNil)

	buf2 := make([]byte, 5)

	test.That(t, client.SetReadDeadline(time.Now().Add(2*time.Second)), test.ShouldBeNil)

	_, err = io.ReadFull(client, buf2)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(buf2), test.ShouldEqual, "world")
}

func TestParseHexIP(t *testing.T) {
	tests := []struct {
		hex  string
		want string
	}{
		{"00000000", "0.0.0.0"},
		{"0100007F", "127.0.0.1"},
		{"0101A8C0", "192.168.1.1"},
		// IPv6 loopback: ::1
		{"00000000000000000000000001000000", "::1"},
		// IPv6 all-zeros: ::
		{"00000000000000000000000000000000", "::"},
		// Non-trivial IPv6: 2001:db8::1; exercises byte reversal in all four groups.
		// Group 0: 2001:0db8 -> bytes 20 01 0d b8 -> LE reversed B8 0D 01 20
		// Group 1: 0000:0000 -> 00000000
		// Group 2: 0000:0000 -> 00000000
		// Group 3: 0000:0001 -> bytes 00 00 00 01 -> LE reversed 01 00 00 00
		{"B80D0120000000000000000001000000", "2001:db8::1"},
		// fe80::1: link-local address.
		// Group 0: fe80:0000 -> bytes fe 80 00 00 -> LE reversed 00 00 80 FE
		// Group 1-2: zeros
		// Group 3: 0000:0001 -> 01000000
		{"000080FE000000000000000001000000", "fe80::1"},
		// Invalid hex: returned as-is.
		{"xyz", "xyz"},
	}

	for _, tc := range tests {
		test.That(t, parseHexIP(tc.hex), test.ShouldEqual, tc.want)
	}
}

func TestParseProcNetTCP(t *testing.T) {
	// Real /proc/net/tcp content with header + entries in various states.
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 67890 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0050 AC10000A:C000 01 00000000:00000000 02:000000D0 00000000  1000        0 11111 2 0000000000000000 20 4 30 10 -1
`

	ports := parseProcNetEntries(content, "tcp", "0A")

	// Should only match the two LISTEN entries (state 0A), not the ESTABLISHED one (state 01).
	test.That(t, len(ports), test.ShouldEqual, 2)

	test.That(t, ports[0].Port, test.ShouldEqual, 8080) // 0x1F90
	test.That(t, ports[0].Host, test.ShouldEqual, "0.0.0.0")
	test.That(t, ports[0].Protocol, test.ShouldEqual, "tcp")
	test.That(t, ports[0].Inode, test.ShouldEqual, 12345)

	test.That(t, ports[1].Port, test.ShouldEqual, 3000) // 0x0BB8
	test.That(t, ports[1].Host, test.ShouldEqual, "127.0.0.1")
	test.That(t, ports[1].Protocol, test.ShouldEqual, "tcp")
	test.That(t, ports[1].Inode, test.ShouldEqual, 67890)
}

func TestParseProcNetUDP(t *testing.T) {
	// UDP bound sockets use state 07.
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:14E9 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 99999 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0035 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 88888 1 0000000000000000 100 0 0 10 0
`

	ports := parseProcNetEntries(content, "udp", "07")

	// Only the bound entry (state 07), not state 01.
	test.That(t, len(ports), test.ShouldEqual, 1)
	test.That(t, ports[0].Port, test.ShouldEqual, 5353) // 0x14E9
	test.That(t, ports[0].Host, test.ShouldEqual, "0.0.0.0")
	test.That(t, ports[0].Protocol, test.ShouldEqual, "udp")
	test.That(t, ports[0].Inode, test.ShouldEqual, 99999)
}

func TestParseProcNetTCP6(t *testing.T) {
	// IPv6 entry for [::]:8080 listening. Lines split to stay under 140 chars.
	header := "  sl  local_address" +
		"                         remote_address" +
		"                        st tx_queue rx_queue" +
		" tr tm->when retrnsmt   uid  timeout inode"
	entry := "   0: 00000000000000000000000000000000:1F90" +
		" 00000000000000000000000000000000:0000 0A" +
		" 00000000:00000000 00:00000000 00000000" +
		"     0        0 54321 1 0000000000000000 100 0 0 10 0"
	content := header + "\n" + entry + "\n"

	ports := parseProcNetEntries(content, "tcp", "0A")

	test.That(t, len(ports), test.ShouldEqual, 1)
	test.That(t, ports[0].Port, test.ShouldEqual, 8080)
	test.That(t, ports[0].Host, test.ShouldEqual, "::")
	test.That(t, ports[0].Protocol, test.ShouldEqual, "tcp")
	test.That(t, ports[0].Inode, test.ShouldEqual, 54321)
}

func TestParseProcNetEmpty(t *testing.T) {
	// Header only, no entries.
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
`

	ports := parseProcNetEntries(content, "tcp", "0A")
	test.That(t, ports, test.ShouldBeEmpty)
}

func TestParseProcNetMalformed(t *testing.T) {
	// Malformed lines should be skipped gracefully.
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: garbage 0A
   short line
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
`

	ports := parseProcNetEntries(content, "tcp", "0A")

	// Only the valid entry should parse.
	test.That(t, len(ports), test.ShouldEqual, 1)
	test.That(t, ports[0].Port, test.ShouldEqual, 8080)
}

func TestAddExplicitPortForward(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	echoPort := startEchoServer(t)

	remoteConn := connectToTestDaemon(t, sockPath)
	daemon := testDaemonForRelay(remoteConn)

	spec := PortForwardSpec{
		RemotePort: echoPort,
		LocalPort:  0, // should default to echoPort
		Protocol:   "tcp",
	}

	// Need a different local port since echo server already has the remote port.
	freeListener, freePort := listenFreePort(t)
	freeListener.Close()

	spec.LocalPort = freePort

	err := daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldBeNil)

	// Verify forward is active and marked explicit.
	statuses := daemon.PortForwardStatuses()
	test.That(t, len(statuses), test.ShouldEqual, 1)
	test.That(t, statuses[0].GetExplicit(), test.ShouldBeTrue)
	test.That(t, statuses[0].GetLocalPort(), test.ShouldEqual, freePort)
	test.That(t, statuses[0].GetRemotePort(), test.ShouldEqual, echoPort)

	// Verify data flows through the forward.
	client, dialErr := (&net.Dialer{}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", freePort))
	test.That(t, dialErr, test.ShouldBeNil)

	defer client.Close()

	_, writeErr := client.Write([]byte("hello"))
	test.That(t, writeErr, test.ShouldBeNil)

	buf := make([]byte, 5)

	_, readErr := io.ReadFull(client, buf)
	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, string(buf), test.ShouldEqual, "hello")
}

func TestAddExplicitPortForwardIdempotent(t *testing.T) {
	daemon := newRemoteDaemon(&noopConnector{})

	// Use a port that won't conflict.
	freeListener, freePort := listenFreePort(t)
	freeListener.Close()

	spec := PortForwardSpec{
		RemotePort: freePort,
		LocalPort:  freePort,
		Protocol:   "tcp",
	}

	// First add succeeds (will conflict since nothing is listening remotely, but
	// the explicit intent is recorded; the listener binds locally).
	// Actually, for this test we just want to verify idempotency of the explicit intent,
	// not the actual relay. Use AddExplicitPortForward which binds locally.
	err := daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldBeNil)

	// Second add is a no-op.
	err = daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(daemon.PortForwardStatuses()), test.ShouldEqual, 1)
}

func TestRemoveExplicitPortForward(t *testing.T) {
	daemon := newRemoteDaemon(&noopConnector{})

	freeListener, freePort := listenFreePort(t)
	freeListener.Close()

	spec := PortForwardSpec{
		RemotePort: freePort,
		LocalPort:  freePort,
		Protocol:   "tcp",
	}

	err := daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(daemon.PortForwardStatuses()), test.ShouldEqual, 1)

	removed := daemon.RemoveExplicitPortForward(spec)
	test.That(t, removed, test.ShouldBeTrue)
	test.That(t, daemon.PortForwardStatuses(), test.ShouldBeEmpty)

	// Removing again returns false.
	removed = daemon.RemoveExplicitPortForward(spec)
	test.That(t, removed, test.ShouldBeFalse)
}

func TestRemoveAutoDetectedPortForwardReturnsFalse(t *testing.T) {
	daemon := newRemoteDaemon(&noopConnector{})

	spec := PortForwardSpec{
		RemotePort: 8080,
		Protocol:   "tcp",
	}

	// No explicit forward was added, so remove should return false.
	removed := daemon.RemoveExplicitPortForward(spec)
	test.That(t, removed, test.ShouldBeFalse)
}

func TestExplicitPortForwardSurvivesReconciliation(t *testing.T) {
	daemon := newRemoteDaemon(&noopConnector{})

	freeListener, freePort := listenFreePort(t)
	freeListener.Close()

	spec := PortForwardSpec{
		RemotePort: freePort,
		LocalPort:  freePort,
		Protocol:   "tcp",
	}

	err := daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(daemon.PortForwardStatuses()), test.ShouldEqual, 1)

	// Reconcile with an empty port list (auto-detection sees nothing).
	// The explicit forward should survive.
	daemon.reconcilePortForwards(t.Context(), nil)

	statuses := daemon.PortForwardStatuses()
	test.That(t, len(statuses), test.ShouldEqual, 1)
	test.That(t, statuses[0].GetExplicit(), test.ShouldBeTrue)
	test.That(t, statuses[0].GetRemotePort(), test.ShouldEqual, freePort)
}

func TestAddExplicitPortForwardConflict(t *testing.T) {
	// Occupy a port.
	listener, port := listenFreePort(t)
	defer listener.Close()

	daemon := newRemoteDaemon(&noopConnector{})
	spec := PortForwardSpec{
		RemotePort: 9999, // doesn't matter
		LocalPort:  port,
		Protocol:   "tcp",
	}

	err := daemon.AddExplicitPortForward(t.Context(), spec)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "already in use")

	// Should not be recorded as explicit.
	test.That(t, daemon.IsExplicitPortForward("tcp", 9999), test.ShouldBeFalse)
}

// listenFreePort binds a TCP listener on a random free port and returns it.
func listenFreePort(t *testing.T) (net.Listener, uint32) {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	test.That(t, err, test.ShouldBeNil)

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	test.That(t, ok, test.ShouldBeTrue)

	return ln, uint32(tcpAddr.Port) //nolint:gosec // test code
}

// noopConnector satisfies the RemoteConnector interface for testing.
type noopConnector struct{}

func (n *noopConnector) Destination() string                              { return noopConnectorName }
func (n *noopConnector) SafeDestination() string                          { return noopConnectorName }
func (n *noopConnector) Identity() string                                 { return "" }
func (n *noopConnector) StateFields() []any                               { return nil }
func (n *noopConnector) Close() error                                     { return nil }
func (n *noopConnector) CopyFile(_ context.Context, _, _, _ string) error { return nil }

func (n *noopConnector) RunOneShotCommand(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (n *noopConnector) DeinitializeRemote(_ context.Context) error       { return nil }
func (n *noopConnector) InitializeRemote(_ context.Context) (bool, error) { return false, nil }

func (n *noopConnector) ConnectToRemoteDaemon(_ context.Context, _, _ string) (RemoteDaemonConnection, bool, error) {
	return nil, false, nil
}

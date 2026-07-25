package graft

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

type fakeListConnectionsServer struct {
	graftv1.UnimplementedGraftServiceServer

	connections map[string]*graftv1.ConnectionStatus
}

func (s *fakeListConnectionsServer) ListConnections(
	_ context.Context, _ *graftv1.ListConnectionsRequest,
) (*graftv1.ListConnectionsResponse, error) {
	return &graftv1.ListConnectionsResponse{Connections: s.connections}, nil
}

func (s *fakeListConnectionsServer) Status(
	_ context.Context, _ *graftv1.StatusRequest,
) (*graftv1.StatusResponse, error) {
	return &graftv1.StatusResponse{Healthy: true}, nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func newTestLocalClient(t *testing.T, server graftv1.GraftServiceServer) (*LocalClient, *bytes.Buffer) {
	t.Helper()

	sockPath := testSocketPath(t, "test.sock")

	var lc net.ListenConfig

	lis, err := lc.Listen(t.Context(), "unix", sockPath)
	test.That(t, err, test.ShouldBeNil)

	grpcServer := grpc.NewServer()
	graftv1.RegisterGraftServiceServer(grpcServer, server)

	go grpcServer.Serve(lis) //nolint:errcheck

	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	test.That(t, err, test.ShouldBeNil)
	t.Cleanup(func() { conn.Close() })

	var outBuf, errBuf bytes.Buffer

	return &LocalClient{
		GraftServiceClient: graftv1.NewGraftServiceClient(conn),
		outWriter:          nopWriteCloser{&outBuf},
		errWriter:          nopWriteCloser{&errBuf},
	}, &outBuf
}

func TestPrintStatusJSON(t *testing.T) {
	t.Run("empty connections", func(t *testing.T) {
		server := &fakeListConnectionsServer{
			connections: map[string]*graftv1.ConnectionStatus{},
		}
		client, outBuf := newTestLocalClient(t, server)

		err := client.PrintStatusJSON(t.Context())
		test.That(t, err, test.ShouldBeNil)

		var result map[string]any
		test.That(t, json.Unmarshal(outBuf.Bytes(), &result), test.ShouldBeNil)
	})

	t.Run("with connections", func(t *testing.T) {
		server := &fakeListConnectionsServer{
			connections: map[string]*graftv1.ConnectionStatus{
				"dev": {
					State:           graftv1.ConnectionState_CONNECTION_STATE_CONNECTED,
					Current:         true,
					SafeDestination: "user@host",
				},
				"staging": {
					State:           graftv1.ConnectionState_CONNECTION_STATE_INITIALIZING,
					SafeDestination: "user@staging",
				},
			},
		}
		client, outBuf := newTestLocalClient(t, server)

		err := client.PrintStatusJSON(t.Context())
		test.That(t, err, test.ShouldBeNil)

		var result map[string]any
		test.That(t, json.Unmarshal(outBuf.Bytes(), &result), test.ShouldBeNil)

		conns, ok := result["connections"].(map[string]any)
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, len(conns), test.ShouldEqual, 2)

		dev, ok := conns["dev"].(map[string]any)
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, dev["current"], test.ShouldBeTrue)
		test.That(t, dev["safe_destination"], test.ShouldEqual, "user@host")
		test.That(t, dev["state"], test.ShouldEqual, "CONNECTION_STATE_CONNECTED")
	})
}

func TestSortConnectionNames(t *testing.T) {
	t.Run("current connection sorts first", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"alpha": {},
			"beta":  {Current: true},
			"gamma": {},
		}
		names := []string{"alpha", "beta", "gamma"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "beta")
		test.That(t, names[1], test.ShouldEqual, "alpha")
		test.That(t, names[2], test.ShouldEqual, "gamma")
	})

	t.Run("alphabetical when no current", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"gamma": {},
			"alpha": {},
			"beta":  {},
		}
		names := []string{"gamma", "alpha", "beta"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "alpha")
		test.That(t, names[1], test.ShouldEqual, "beta")
		test.That(t, names[2], test.ShouldEqual, "gamma")
	})

	t.Run("current first then alphabetical", func(t *testing.T) {
		connections := map[string]*graftv1.ConnectionStatus{
			"zulu":  {Current: true},
			"alpha": {},
			"beta":  {},
		}
		names := []string{"zulu", "alpha", "beta"}

		sortConnectionNames(names, connections)

		test.That(t, names[0], test.ShouldEqual, "zulu")
		test.That(t, names[1], test.ShouldEqual, "alpha")
		test.That(t, names[2], test.ShouldEqual, "beta")
	})
}

type fakeListCommandsServer struct {
	graftv1.UnimplementedGraftServiceServer

	commands []*graftv1.CommandInfo
}

func (s *fakeListCommandsServer) ListCommands(
	_ context.Context, _ *graftv1.ListCommandsRequest,
) (*graftv1.ListCommandsResponse, error) {
	return &graftv1.ListCommandsResponse{Commands: s.commands}, nil
}

func TestPrintManagedCommandsReturnsUntypedNil(t *testing.T) {
	client, outBuf := newTestLocalClient(t, &fakeListCommandsServer{
		commands: []*graftv1.CommandInfo{
			{CommandId: "abcd1234", ConnectionName: "dev", Command: "npm run dev", Running: true},
			{CommandId: "ffff0000", ConnectionName: "dev", Command: "make", ExitStatus: 2},
		},
	})

	err := client.PrintManagedCommands(t.Context(), "")

	// The == comparison is the regression check for the `graft ps` panic: a
	// typed-nil *errors.Error would pass reflection-based nil assertions but
	// still crash callers that treat it as a real error.
	test.That(t, err == nil, test.ShouldBeTrue)

	out := outBuf.String()
	test.That(t, out, test.ShouldContainSubstring, "abcd1234")
	test.That(t, out, test.ShouldContainSubstring, "detached")
	test.That(t, out, test.ShouldContainSubstring, "exited(2)")
}

type fakeSignalCommand struct {
	signals chan string
}

func (f *fakeSignalCommand) Stdin() io.WriteCloser             { return nopWriteCloser{io.Discard} }
func (f *fakeSignalCommand) Stdout() io.Reader                 { return strings.NewReader("") }
func (f *fakeSignalCommand) Stderr() io.Reader                 { return strings.NewReader("") }
func (f *fakeSignalCommand) Wait() (int, error)                { return 0, nil }
func (f *fakeSignalCommand) Release()                          {}
func (f *fakeSignalCommand) SetEnvVar(_, _ string) error       { return nil }
func (f *fakeSignalCommand) NotifyWindowChange(_, _ int) error { return nil }

func (f *fakeSignalCommand) Signal(sig string) error {
	f.signals <- sig

	return nil
}

func TestForwardSignalsToCommand(t *testing.T) {
	cmd := &fakeSignalCommand{signals: make(chan string, 8)}
	sigs := make(chan os.Signal, 8)
	terminated := make(chan int, 1)

	go forwardSignalsToCommand(t.Context(), cmd, sigs, terminated)

	expectForward := func(sig os.Signal, want string) {
		t.Helper()

		sigs <- sig

		select {
		case got := <-cmd.signals:
			test.That(t, got, test.ShouldEqual, want)
		case <-time.After(5 * time.Second):
			t.Fatalf("%v was not forwarded", sig)
		}
	}

	// Every app-facing signal forwards to the command, like a local
	// foreground process, without ending the local wait.
	expectForward(syscall.SIGINT, SignalInterrupt)
	expectForward(syscall.SIGQUIT, SignalQuit)
	expectForward(syscall.SIGUSR1, SignalUser1)
	expectForward(syscall.SIGUSR2, SignalUser2)

	select {
	case <-terminated:
		t.Fatal("forwarded signals must not terminate the handler")
	default:
	}

	// SIGTERM forwards and also releases the handler so the CLI stays killable.
	expectForward(syscall.SIGTERM, SignalTerminate)

	select {
	case code := <-terminated:
		test.That(t, code, test.ShouldEqual, 143)
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not release the handler")
	}

	close(sigs)
}

func TestForwardSignalsHangupDetachesWithoutForwarding(t *testing.T) {
	// The terminal going away is a detach: forwarding the HUP would kill a
	// kept command and defeat its persistence.
	cmd := &fakeSignalCommand{signals: make(chan string, 8)}
	sigs := make(chan os.Signal, 8)
	terminated := make(chan int, 1)

	go forwardSignalsToCommand(t.Context(), cmd, sigs, terminated)

	sigs <- syscall.SIGHUP

	select {
	case code := <-terminated:
		test.That(t, code, test.ShouldEqual, 129)
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not release the handler")
	}

	select {
	case sig := <-cmd.signals:
		t.Fatalf("SIGHUP must not be forwarded, but %q was sent", sig)
	default:
	}

	close(sigs)
}

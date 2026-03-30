package graft

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"

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

package graft

import (
	"context"
	"encoding/json"
	"testing"

	"go.viam.com/test"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func TestConnectionNameHint(t *testing.T) {
	t.Run("extracts hint from ErrorInfo details", func(t *testing.T) {
		statusErr, err := status.New(codes.InvalidArgument, "failed to find an eligible connection").
			WithDetails(&errdetails.ErrorInfo{
				Metadata: map[string]string{errors.ErrorMetadataFieldConnectionNameHint: "prod"},
			})
		test.That(t, err, test.ShouldBeNil)

		test.That(t, connectionNameHint(statusErr.Err()), test.ShouldEqual, "prod")
	})

	t.Run("wrapped grpc status still yields the hint", func(t *testing.T) {
		statusErr, err := status.New(codes.InvalidArgument, "failed to find an eligible connection").
			WithDetails(&errdetails.ErrorInfo{
				Metadata: map[string]string{errors.ErrorMetadataFieldConnectionNameHint: "prod"},
			})
		test.That(t, err, test.ShouldBeNil)

		test.That(t, connectionNameHint(errors.Wrap(statusErr.Err())), test.ShouldEqual, "prod")
	})

	t.Run("plain error has no hint", func(t *testing.T) {
		test.That(t, connectionNameHint(errors.New("failed to find an eligible connection")), test.ShouldEqual, "")
	})
}

func TestFormatClaudeSessionStartContext(t *testing.T) {
	t.Run("connected with active and inactive shims and a port forward", func(t *testing.T) {
		cs := &graftv1.ConnectionStatus{
			State:           graftv1.ConnectionState_CONNECTION_STATE_CONNECTED,
			SafeDestination: "user@host",
			PortForwardStatuses: []*graftv1.PortForwardStatus{
				{Protocol: "tcp", RemotePort: 8080, LocalPort: 8080},
				{Protocol: "tcp", RemotePort: 5432, Conflict: true, ConflictReason: "already in use"},
			},
		}
		destCommands := map[string]*graftv1.CommandForwardings{
			"dev": {Commands: []*graftv1.CommandForwarding{
				{Local: "cargo", Remote: "cargo", Active: true},
				{Local: "make", Remote: "make", Active: false},
			}},
		}

		text := formatClaudeSessionStartContext("dev", cs, destCommands)

		test.That(t, text, test.ShouldContainSubstring, "dev")
		test.That(t, text, test.ShouldContainSubstring, "user@host")
		test.That(t, text, test.ShouldContainSubstring, "connected")
		test.That(t, text, test.ShouldContainSubstring, "cargo")
		test.That(t, text, test.ShouldContainSubstring, "make")
		test.That(t, text, test.ShouldContainSubstring, "graft run --to dev")
		test.That(t, text, test.ShouldContainSubstring, "sudo")
		test.That(t, text, test.ShouldContainSubstring, "localhost:8080")
		test.That(t, text, test.ShouldContainSubstring, "CONFLICT")
		test.That(t, text, test.ShouldContainSubstring, "already in use")
	})

	t.Run("state reason is surfaced", func(t *testing.T) {
		reason := "connection refused"
		cs := &graftv1.ConnectionStatus{
			State:           graftv1.ConnectionState_CONNECTION_STATE_FAILED,
			SafeDestination: "user@host",
			StateReason:     &reason,
		}

		text := formatClaudeSessionStartContext("dev", cs, nil)

		test.That(t, text, test.ShouldContainSubstring, "failed")
		test.That(t, text, test.ShouldContainSubstring, "connection refused")
	})

	t.Run("no shimmed commands still suggests graft run", func(t *testing.T) {
		cs := &graftv1.ConnectionStatus{
			State:           graftv1.ConnectionState_CONNECTION_STATE_CONNECTED,
			SafeDestination: "user@host",
		}

		text := formatClaudeSessionStartContext("dev", cs, nil)

		test.That(t, text, test.ShouldContainSubstring, "graft run --to dev")
	})
}

func TestFormatConnectionHintContext(t *testing.T) {
	text := formatConnectionHintContext("prod")
	test.That(t, text, test.ShouldContainSubstring, "prod")
	test.That(t, text, test.ShouldContainSubstring, "graft use prod")
	test.That(t, text, test.ShouldContainSubstring, "graft run --to prod")
}

type fakeClaudeHookServer struct {
	graftv1.UnimplementedGraftServiceServer

	selectErr    error
	connName     string
	connStatus   *graftv1.ConnectionStatus
	destCommands map[string]*graftv1.CommandForwardings
}

func (s *fakeClaudeHookServer) SessionSelectConnection(
	_ context.Context, _ *graftv1.SessionSelectConnectionRequest,
) (*graftv1.SessionSelectConnectionResponse, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}

	return &graftv1.SessionSelectConnectionResponse{ConnectionName: s.connName}, nil
}

func (s *fakeClaudeHookServer) SessionShimmedCommands(
	_ context.Context, _ *graftv1.SessionShimmedCommandsRequest,
) (*graftv1.SessionShimmedCommandsResponse, error) {
	return &graftv1.SessionShimmedCommandsResponse{DestinationCommands: s.destCommands}, nil
}

func (s *fakeClaudeHookServer) ListConnections(
	_ context.Context, _ *graftv1.ListConnectionsRequest,
) (*graftv1.ListConnectionsResponse, error) {
	return &graftv1.ListConnectionsResponse{
		Connections: map[string]*graftv1.ConnectionStatus{s.connName: s.connStatus},
	}, nil
}

func TestPrintClaudeSessionStartHook(t *testing.T) {
	t.Run("no eligible connection prints nothing", func(t *testing.T) {
		server := &fakeClaudeHookServer{selectErr: errors.New("failed to find an eligible connection")}
		client, outBuf := newTestLocalClient(t, server)

		err := client.PrintClaudeSessionStartHook(t.Context())
		test.That(t, err, test.ShouldBeNil)
		test.That(t, outBuf.String(), test.ShouldBeEmpty)
	})

	t.Run("hint error prints a hook payload naming the hinted connection", func(t *testing.T) {
		statusErr, wrapErr := status.New(codes.InvalidArgument, "failed to find an eligible connection").
			WithDetails(&errdetails.ErrorInfo{
				Metadata: map[string]string{errors.ErrorMetadataFieldConnectionNameHint: "prod"},
			})
		test.That(t, wrapErr, test.ShouldBeNil)

		server := &fakeClaudeHookServer{selectErr: statusErr.Err()}
		client, outBuf := newTestLocalClient(t, server)

		err := client.PrintClaudeSessionStartHook(t.Context())
		test.That(t, err, test.ShouldBeNil)

		var payload claudeSessionStartHookOutput
		test.That(t, json.Unmarshal(outBuf.Bytes(), &payload), test.ShouldBeNil)
		test.That(t, payload.HookSpecificOutput.HookEventName, test.ShouldEqual, "SessionStart")
		test.That(t, payload.HookSpecificOutput.AdditionalContext, test.ShouldContainSubstring, "prod")
	})

	t.Run("resolved connection prints a hook payload describing it", func(t *testing.T) {
		server := &fakeClaudeHookServer{
			connName: "dev",
			connStatus: &graftv1.ConnectionStatus{
				State:           graftv1.ConnectionState_CONNECTION_STATE_CONNECTED,
				SafeDestination: "user@host",
			},
			destCommands: map[string]*graftv1.CommandForwardings{
				"dev": {Commands: []*graftv1.CommandForwarding{
					{Local: "cargo", Remote: "cargo", Active: true},
				}},
			},
		}
		client, outBuf := newTestLocalClient(t, server)

		err := client.PrintClaudeSessionStartHook(t.Context())
		test.That(t, err, test.ShouldBeNil)

		var payload claudeSessionStartHookOutput
		test.That(t, json.Unmarshal(outBuf.Bytes(), &payload), test.ShouldBeNil)
		test.That(t, payload.HookSpecificOutput.HookEventName, test.ShouldEqual, "SessionStart")
		test.That(t, payload.HookSpecificOutput.AdditionalContext, test.ShouldContainSubstring, "dev")
		test.That(t, payload.HookSpecificOutput.AdditionalContext, test.ShouldContainSubstring, "cargo")
	})
}

package graft

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// commandFanoutTimeout bounds how long a local daemon waits on any one
// connection while listing or locating commands.
const commandFanoutTimeout = 5 * time.Second

// ListCommands lists managed commands. On a remote daemon these are its own;
// on a local daemon results are gathered across (optionally one of) its
// connections.
func (srv *Server) ListCommands(
	ctx context.Context,
	req *graftv1.ListCommandsRequest,
) (*graftv1.ListCommandsResponse, error) {
	if srv.role == ServerRoleRemote {
		return &graftv1.ListCommandsResponse{Commands: srv.cmdRegistry.List()}, nil
	}

	if req.GetConnectionName() != "" {
		// Surface a bad --to instead of silently listing nothing.
		if _, err := srv.connMgr.Connection(req.GetConnectionName()); err != nil {
			return nil, err
		}
	}

	conns := srv.connMgr.Connections()

	var commands []*graftv1.CommandInfo

	for name, conn := range conns {
		if req.GetConnectionName() != "" && name != req.GetConnectionName() {
			continue
		}

		connCommands, err := listConnectionCommands(ctx, conn)
		if err != nil {
			if req.GetConnectionName() != "" {
				return nil, err
			}

			slog.DebugContext(ctx, "error listing commands for connection", "connection", name, "error", err)

			continue
		}

		commands = append(commands, connCommands...)
	}

	return &graftv1.ListCommandsResponse{Commands: commands}, nil
}

// listConnectionCommands lists a single connection's managed commands,
// annotated with the connection name.
func listConnectionCommands(ctx context.Context, conn *Connection) ([]*graftv1.CommandInfo, error) {
	if state, _ := conn.State(); state != ConnectionStateConnected {
		return nil, errors.WrapSuffix(errConnectionNotConnected, conn.Name())
	}

	remClient, err := conn.remoteServiceClient()
	if err != nil {
		return nil, err
	}

	listCtx, cancel := context.WithTimeout(ctx, commandFanoutTimeout)
	defer cancel()

	resp, err := remClient.ListCommands(listCtx, &graftv1.ListCommandsRequest{})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	commands := resp.GetCommands()
	for _, info := range commands {
		info.ConnectionName = conn.Name()
	}

	return commands, nil
}

// KillCommand signals a managed command's process group.
func (srv *Server) KillCommand(
	ctx context.Context,
	req *graftv1.KillCommandRequest,
) (*graftv1.KillCommandResponse, error) {
	sig := req.GetSignal()
	if sig == "" {
		sig = SignalTerminate
	}

	if srv.role == ServerRoleRemote {
		managed, ok := srv.cmdRegistry.Get(req.GetCommandId())
		if !ok {
			return nil, errors.Wrap(status.Error(codes.NotFound, "no such command: "+req.GetCommandId()))
		}

		if exitStatus, exited, _ := managed.ExitStatus(); exited { //nolint:errcheck // liveness peek only
			// Nothing left to signal; report the exit and forget the entry.
			srv.cmdRegistry.Remove(req.GetCommandId())

			return &graftv1.KillCommandResponse{WasExited: true, ExitStatus: int64(exitStatus)}, nil
		}

		if req.GetEscalate() {
			managed.KillWithEscalation()

			return &graftv1.KillCommandResponse{}, nil
		}

		if err := managed.Signal(sig); err != nil {
			return nil, err
		}

		return &graftv1.KillCommandResponse{}, nil
	}

	conn, err := srv.findCommandConnection(ctx, req.GetConnectionName(), req.GetCommandId())
	if err != nil {
		return nil, err
	}

	remClient, err := conn.remoteServiceClient()
	if err != nil {
		return nil, err
	}

	resp, err := remClient.KillCommand(ctx, &graftv1.KillCommandRequest{
		CommandId: req.GetCommandId(),
		Signal:    sig,
		Escalate:  req.GetEscalate(),
	})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return resp, nil
}

// findCommandConnection resolves which connection owns a command: directly by
// name when given, otherwise by asking every connected connection.
func (srv *Server) findCommandConnection(
	ctx context.Context,
	connectionName, commandID string,
) (*Connection, error) {
	if connectionName != "" {
		return srv.connMgr.Connection(connectionName)
	}

	for name, conn := range srv.connMgr.Connections() {
		commands, err := listConnectionCommands(ctx, conn)
		if err != nil {
			slog.DebugContext(ctx, "error listing commands for connection", "connection", name, "error", err)

			continue
		}

		for _, info := range commands {
			if info.GetCommandId() == commandID {
				return conn, nil
			}
		}
	}

	return nil, errors.Wrap(status.Error(codes.NotFound, "no connection has command: "+commandID))
}

// DetachCommand disconnects a managed command's attached client, leaving the
// command running (it is flipped to keep persistence: an explicit detach
// means "let it run").
func (srv *Server) DetachCommand(
	ctx context.Context,
	req *graftv1.DetachCommandRequest,
) (*graftv1.DetachCommandResponse, error) {
	if srv.role == ServerRoleRemote {
		managed, ok := srv.cmdRegistry.Get(req.GetCommandId())
		if !ok {
			return nil, errors.Wrap(status.Error(codes.NotFound, "no such command: "+req.GetCommandId()))
		}

		return &graftv1.DetachCommandResponse{WasAttached: managed.DetachAndKeep()}, nil
	}

	conn, err := srv.findCommandConnection(ctx, req.GetConnectionName(), req.GetCommandId())
	if err != nil {
		return nil, err
	}

	remClient, err := conn.remoteServiceClient()
	if err != nil {
		return nil, err
	}

	resp, err := remClient.DetachCommand(ctx, &graftv1.DetachCommandRequest{
		CommandId: req.GetCommandId(),
	})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return resp, nil
}

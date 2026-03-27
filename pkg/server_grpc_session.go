package graft

import (
	"context"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// SessionReportCWD informs the daemon of a session's current working directory so that it can be mirrored on the remote.
func (srv *Server) SessionReportCWD(ctx context.Context, req *graftv1.SessionReportCWDRequest) (*graftv1.SessionReportCWDResponse, error) {
	err := srv.sessMgr.UpdateSessionCWD(ctx, req.GetPid(), req.GetCwd())
	if err != nil {
		return nil, err
	}

	return &graftv1.SessionReportCWDResponse{}, nil
}

// SessionWhich asks which connection can handle the given command.
func (srv *Server) SessionWhich(_ context.Context, req *graftv1.SessionWhichRequest) (*graftv1.SessionWhichResponse, error) {
	sess, err := srv.sessMgr.SessionByPID(req.GetPid())
	if err != nil {
		return nil, err
	}

	cmd, err := srv.sessMgr.Which(sess, req.GetCommand())
	if err != nil {
		return nil, err
	}

	return &graftv1.SessionWhichResponse{ConnectionName: cmd.conn.Name(), RemotePath: cmd.path}, nil
}

// SessionSelectConnection returns the best connection for session.
func (srv *Server) SessionSelectConnection(
	ctx context.Context,
	req *graftv1.SessionSelectConnectionRequest,
) (*graftv1.SessionSelectConnectionResponse, error) {
	updateErr := srv.sessMgr.UpdateSessionCWD(ctx,
		req.GetPid(), req.GetCwd())
	if updateErr != nil {
		return nil, updateErr
	}

	sess, err := srv.sessMgr.SessionByPID(req.GetPid())
	if err != nil {
		return nil, err
	}

	conn, err := srv.sessMgr.selectConnection(ctx, sess, "", req.GetCwd())
	if err != nil {
		return nil, err
	}

	resp := &graftv1.SessionSelectConnectionResponse{
		ConnectionName: conn.Name(),
	}

	// Include LocalRoot+RemoteRoot as a path remapping.
	if localRoot, remoteRoot := conn.Roots(); localRoot != "" && remoteRoot != "" {
		resp.PathRemappings = append(resp.PathRemappings, &graftv1.PathRemapping{
			FromPrefix: localRoot,
			ToPrefix:   remoteRoot,
		})
	}

	for _, syncIntent := range conn.Synchronizations() {
		resp.PathRemappings = append(resp.PathRemappings, &graftv1.PathRemapping{
			FromPrefix: syncIntent.FromLocal,
			ToPrefix:   syncIntent.ToRemote,
		})
	}

	return resp, nil
}

// SessionPinConnection pins a connection to a session, overriding CWD-based auto-selection.
func (srv *Server) SessionPinConnection(
	ctx context.Context,
	req *graftv1.SessionPinConnectionRequest,
) (*graftv1.SessionPinConnectionResponse, error) {
	connName, err := srv.sessMgr.PinConnection(ctx, req.GetPid(), req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	return &graftv1.SessionPinConnectionResponse{ConnectionName: connName}, nil
}

// SessionShimmedCommands returns all commands that should be shimmed for a session across all established connections.
func (srv *Server) SessionShimmedCommands(
	ctx context.Context,
	req *graftv1.SessionShimmedCommandsRequest,
) (*graftv1.SessionShimmedCommandsResponse, error) {
	sess, err := srv.sessMgr.SessionByPID(req.GetPid())
	if err != nil {
		return nil, err
	}

	resolvedConn, _ := srv.sessMgr.resolveSessionConnection(ctx, sess)
	fwdings := srv.sessMgr.DesiredForwardingsForSession(ctx, sess, resolvedConn)

	destCommands := make(map[string]*graftv1.CommandForwardings, len(fwdings))

	for dest, fwds := range fwdings {
		commands := make([]*graftv1.CommandForwarding, 0, len(fwds))

		for _, fwd := range fwds {
			localName := fwd.LocalName(dest)
			commands = append(commands, &graftv1.CommandForwarding{
				Local:  localName,
				Remote: fwd.Name,
			})
		}

		destCommands[dest] = &graftv1.CommandForwardings{Commands: commands}
	}

	return &graftv1.SessionShimmedCommandsResponse{
		DestinationCommands: destCommands,
	}, nil
}

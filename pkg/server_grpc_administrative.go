package graft

import (
	"context"
	"fmt"
	"log/slog"
	"syscall"
	"time"

	"github.com/fatih/color"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// Status returns high-level information about the daemon (local/remote) such
// as its health, version, and recent logs.
func (srv *Server) Status(ctx context.Context, req *graftv1.StatusRequest) (*graftv1.StatusResponse, error) {
	if req.ConnectionName == nil {
		return &graftv1.StatusResponse{
			Healthy:     true,
			VersionInfo: BuildVersion(),
			RecentLogs:  srv.buffLineWriter.Lines(),
			Uptime:      durationpb.New(time.Since(srv.startedAt)),
		}, nil
	}

	conn, err := srv.connMgr.connection(req.GetConnectionName(), true)
	if err != nil {
		return nil, err
	}

	remClient := graftv1.NewGraftServiceClient(conn.daemon.RemoteClientConn())

	status, err := remClient.Status(ctx, &graftv1.StatusRequest{})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return status, nil
}

// Ping prints a pong message over the OOB channel.
func (srv *Server) Ping(ctx context.Context, req *graftv1.PingRequest) (*graftv1.PingResponse, error) {
	now := time.Now()
	fmt.Fprintf(OOBWriterFromContext(ctx),
		"%s=%s\n", color.RedString("local->remote"), now.Sub(time.Unix(0, req.GetLocalTimeUnixNanos())).String())

	return &graftv1.PingResponse{
		LocalTimeUnixNanos: now.UnixNano(),
	}, nil
}

// Shutdown shuts this daemon down.
func (srv *Server) Shutdown(_ context.Context, _ *graftv1.ShutdownRequest) (*graftv1.ShutdownResponse, error) {
	return &graftv1.ShutdownResponse{}, killSelf()
}

// Restart initiates a restart for this daemon.
func (srv *Server) Restart(_ context.Context, _ *graftv1.RestartRequest) (*graftv1.RestartResponse, error) {
	srv.serverMu.Lock()
	srv.restartRequested = true
	srv.serverMu.Unlock()

	return &graftv1.RestartResponse{}, killSelf()
}

// OOBMessages subscribes to a broadcast stream of out-of-band messages that this daemon
// can use to send clients information not strictly related to forwarded commands.
//
// TODO(erd): Evaluate if the correlation id concept is needed. It doesn't seem like it is
// and the demuxing could be eliminated without it.
func (srv *Server) OOBMessages(_ *graftv1.OOBMessagesRequest, server graftv1.GraftService_OOBMessagesServer) error {
	listener, doneWithListener := srv.listenToOOB()
	defer doneWithListener()

	// ACK
	err := server.Send(&graftv1.OOBMessagesResponse{
		CorrelationId: "",
	})
	if err != nil {
		return errors.Wrap(err)
	}

	for {
		select {
		case <-server.Context().Done():
			return nil
		case msgs := <-listener.messages:
			err := server.Send(&graftv1.OOBMessagesResponse{
				CorrelationId: msgs.coID,
				Messages:      msgs.data,
			})
			if err != nil {
				slog.DebugContext(server.Context(), "error sending oob message", "error", err)
			}
		}
	}
}

// killSelf sends an INTERRUPT signal to our own process; this doesn't really kill anything but it should start
// the process.
func killSelf() error {
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

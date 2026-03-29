package graft

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// LocalClient is the connection from a local graft client to a local graft daemon.
// For logic that is implemented on the daemon side, out-of-band messages are supported.
// This makes the client lightweight while not missing out on any important messages that
// come form the daemon.
//
// The connection is gRPC over a unix domain socket. Right now, only one daemon is assumed to
// be on a host and it lives at DaemonSocketPath(). For out-of-band messages, they are broadcasted
// out to all connected clients and must be de-muxed by a coorelation id assigned at client construction
// time. This could probably be simplified in the future.
type LocalClient struct {
	*grpc.ClientConn
	graftv1.GraftServiceClient

	outWriter             io.WriteCloser
	errWriter             io.WriteCloser
	handleErrorMiddleware func(err error) error
	logger                *slog.Logger

	activeWorkers sync.WaitGroup
	cancel        func()
}

// ErrDaemonNotRunning is returned when the daemon socket does not exist.
var ErrDaemonNotRunning = errors.New("graft daemon is not running (start it with 'graft daemon')")

// ConnectAndCheck dials the daemon socket and makes a Status health check. It returns the gRPC
// connection, service client, and the daemon's version info on success.
// Returns ErrDaemonNotRunning if the socket does not exist.
func ConnectAndCheck(
	ctx context.Context, sockPath string,
) (*grpc.ClientConn, graftv1.GraftServiceClient, *graftv1.VersionInfo, error) {
	if _, err := os.Stat(sockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, ErrDaemonNotRunning
		}

		return nil, nil, nil, errors.Wrap(err)
	}

	clientConn, err := grpc.NewClient(
		"unix://"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err)
	}

	svcClient := graftv1.NewGraftServiceClient(clientConn)

	checkCtx, checkCancel := context.WithTimeout(ctx, 3*time.Second)
	defer checkCancel()

	resp, err := svcClient.Status(checkCtx, &graftv1.StatusRequest{})
	if err != nil {
		clientConn.Close()

		return nil, nil, nil, errors.Wrap(err)
	}

	return clientConn, svcClient, resp.GetVersionInfo(), nil
}

// WaitForDaemon polls the daemon at the given socket path until a healthy connection can be
// established or the context is cancelled.
func WaitForDaemon(ctx context.Context, sockPath string) error {
	const pollInterval = 250 * time.Millisecond

	for {
		conn, _, _, err := ConnectAndCheck(ctx, sockPath)
		if err == nil {
			conn.Close()

			return nil
		}

		select {
		case <-ctx.Done():
			return errors.Wrap(context.Cause(ctx))
		case <-time.After(pollInterval):
		}
	}
}

// NewLocalClient returns a client connected to the local graft daemon. The returned context
// should be used for all requests made to the daemon; it is the child of the given context.
func NewLocalClient(
	ctx context.Context,
	outWriter io.WriteCloser,
	errWriter io.WriteCloser,
	handleErr func(err error) error,
	withOOBMsgs bool,
	logger *slog.Logger,
) (*LocalClient, context.Context, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	sockPath, err := DaemonSocketPathForCurrentHost(ServerRoleLocal)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	clientConn, svcClient, _, err := ConnectAndCheck(cancelCtx, sockPath)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	rpcCtx := cancelCtx

	if handleErr == nil {
		handleErr = func(err error) error { return err }
	}

	client := &LocalClient{
		ClientConn:            clientConn,
		GraftServiceClient:    svcClient,
		cancel:                cancel,
		outWriter:             outWriter,
		errWriter:             errWriter,
		handleErrorMiddleware: handleErr,
		logger:                logger,
	}
	if withOOBMsgs {
		coID, err := client.listenOOB(cancelCtx, errWriter)
		if err != nil {
			cancel()

			return nil, nil, err
		}

		rpcCtx = metadata.NewOutgoingContext(cancelCtx, metadata.MD{
			CorrelationIDHeaderName: []string{coID},
		})
	}

	return client, rpcCtx, nil
}

func (client *LocalClient) Close() {
	client.cancel()
	client.activeWorkers.Wait()

	if err := client.ClientConn.Close(); err != nil {
		client.logger.ErrorContext(context.Background(), "error closing gRPC connection", "error", err)
	}
}

func (client *LocalClient) listenOOB(ctx context.Context, writer io.Writer) (string, error) {
	coID := uuid.NewString()

	oobClient, err := client.OOBMessages(ctx, &graftv1.OOBMessagesRequest{})
	if err != nil {
		return "", errors.Wrap(err)
	}

	client.activeWorkers.Add(1)
	client.activeWorkers.Go(func() {
		if _, err := io.Copy(writer, demuxOOB(ctx, oobClient, coID, client.activeWorkers.Done)); err != nil {
			client.logger.ErrorContext(ctx, "error processing oob channel", "error", err)
		}
	})

	return coID, nil
}

// demuxOOB demuxes OOB messages from the daemon and returns once any data has been received.
func demuxOOB(
	ctx context.Context,
	oobClient grpc.ServerStreamingClient[graftv1.OOBMessagesResponse],
	coID string,
	onDone func(),
) io.Reader {
	err := oobClient.CloseSend()
	if err != nil {
		slog.ErrorContext(ctx, "error closing OOB client", "co_id", coID, "error", err)
	}

	reader, writer := io.Pipe()
	firstRecv := make(chan struct{})

	go func() {
		defer onDone()
		defer writer.Close()

		var firstRecvOnce bool

		for {
			if context.Cause(ctx) != nil {
				return
			}

			msg, err := oobClient.Recv()

			if !firstRecvOnce {
				firstRecvOnce = true

				close(firstRecv)
			}

			if err != nil {
				return
			}

			if msg.GetCorrelationId() != coID {
				continue
			}
			//nolint:errcheck
			writer.Write(msg.GetMessages())
		}
	}()

	select {
	case <-ctx.Done():
		return bytes.NewReader(nil)
	case <-firstRecv:
	}

	return reader
}

func (client *LocalClient) handleError(err error) error {
	finalErr := err
	if s := status.Convert(finalErr); s != nil {
		if s.Code() == codes.Unavailable {
			finalErr = errors.New("lost connection to daemon")
		}
	}

	return client.handleErrorMiddleware(finalErr)
}

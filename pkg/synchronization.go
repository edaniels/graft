package graft

import (
	"bytes"
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/endpoint/remote"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// TODO(erd): Determine if sync protocol numbers need to persist across daemon restarts.
var nextSyncProtoNum atomic.Int32

func init() {
	nextSyncProtoNum.Store(10)
}

// A SynchronizationIntent is (currently) a bi-directional file synchronization between a local source and a remote destination.
// Given it's bidirectional, and based off of https://github.com/mutagen-io/mutagen (no SSPL use here), you can consider
// these to be alpha and beta endpoints. More synchronization options should be supported in the future.
//
// This is not safe for concurrent use.
type SynchronizationIntent struct {
	FromLocal string
	ToRemote  string
}

// SynchronizationIntentFromConfig returns a SynchronizationIntent to be used internally from the config API.
//
// Assumed to be validated.
func SynchronizationIntentFromConfig(conf SynchronizationIntentConfig) SynchronizationIntent {
	return SynchronizationIntent(conf)
}

// AsConfig converts the intent into a config format suitable for (de)serialization.
func (i SynchronizationIntent) AsConfig() SynchronizationIntentConfig {
	return SynchronizationIntentConfig(i)
}

// ConnRemoteClientConnFromContext returns a connection's backing gRPC connection via the given context.
//
// See: ContextWithConnRemoteClientConn.
func ConnRemoteClientConnFromContext(ctx context.Context) (*grpc.ClientConn, error) {
	conn, ok := ctx.Value(ctxKeyConnRemoteClientConn).(*grpc.ClientConn)
	if !ok {
		return nil, errors.New("expected connection's remote client connection in context")
	}

	return conn, nil
}

// ContextWithConnRemoteClientConn associates the given connection's backing gRPC connection with the context.
// This is used to transit the connection through the graft<->mutagen API boundary based on how mutagen protocol
// handlers work.
func ContextWithConnRemoteClientConn(ctx context.Context, conn *grpc.ClientConn) context.Context {
	return context.WithValue(ctx, ctxKeyConnRemoteClientConn, conn)
}

type mutagenSyncProtocolHandler struct {
	server *Server
}

// Connect implements [synchronization.ProtocolHandler] for mutagen. The destination is completely ignored
// since the destination is considered to be the connection available via the context variable.
func (handler *mutagenSyncProtocolHandler) Connect(
	ctx context.Context,
	logger *logging.Logger,
	url *urlpkg.URL,
	_ string,
	session string,
	version synchronization.Version,
	configuration *synchronization.Configuration,
	alpha bool,
) (synchronization.Endpoint, error) {
	if url.GetKind() != urlpkg.Kind_Synchronization {
		panic("non-synchronization URL dispatched to synchronization protocol handler")
	} else if url.GetProtocol() != urlpkg.Protocol(handler.server.syncProtoNum) { //nolint:gosec // overflow okay
		panic("non-graft URL dispatched to graft protocol handler")
	}

	remoteConn, err := ConnRemoteClientConnFromContext(ctx)
	if err != nil {
		// TODO(erd): Document under what conditions the remote client conn is unavailable and verify fallback behavior.
		slog.ErrorContext(ctx, "failed to get remote client conn from context; trying from conn itself", "error", err)

		slog.DebugContext(ctx, "getting connection for endpoint", "name", url.GetHost())

		conn, connectErr := handler.server.connMgr.Connection(url.GetHost())
		if connectErr != nil {
			return nil, connectErr
		}

		// Document the deadlock scenario. This probably doesn't happen when the sync manager is retrying.
		// Note(erd): Original deadlock scenario unclear; verify before modifying sync retry logic.
		remoteConn = conn.daemon.RemoteClientConn()
	}

	client := graftv1.NewGraftServiceClient(remoteConn)

	syncClient, err := client.SyncFilesToConnectionProtocol(ctx)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	stream := newMutagenSyncStreamClientWrapper(syncClient)

	endpoint, err := remote.NewEndpoint(logger, stream, url.GetPath(), session, version, configuration, alpha)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return endpoint, nil
}

type binaryDataMessage interface {
	GetData() []byte
}

type mutagenSyncStreamer[ReadT binaryDataMessage] interface {
	Recv() (ReadT, error)
}

type mutagenSyncStreamWrapper[ReadT binaryDataMessage] struct {
	streamer mutagenSyncStreamer[ReadT]
	buf      bytes.Buffer
	send     func(data []byte) error
	close    func() error
}

func (w *mutagenSyncStreamWrapper[ReadT]) Read(p []byte) (int, error) {
	if w.buf.Len() != 0 {
		n, err := w.buf.Read(p)
		if err == nil {
			return n, nil
		}
	}

	resp, err := w.streamer.Recv()
	if err != nil {
		return 0, errors.Wrap(err)
	}

	_, err = w.buf.Write(resp.GetData())
	if err != nil {
		return 0, errors.Wrap(err)
	}

	return w.Read(p)
}

func (w *mutagenSyncStreamWrapper[ReadT]) Write(p []byte) (int, error) {
	err := w.send(p)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (w *mutagenSyncStreamWrapper[ReadT]) Close() error {
	return w.close()
}

// newMutagenSyncStreamClientWrapper is used to handle the local to remote daemon part of the mutagen file sync protocol.
// It simply wraps the binary data from both sides in proto messages.
func newMutagenSyncStreamClientWrapper(
	syncClient graftv1.GraftService_SyncFilesToConnectionProtocolClient,
) *mutagenSyncStreamWrapper[*graftv1.SyncFilesToConnectionProtocolResponse] {
	return &mutagenSyncStreamWrapper[*graftv1.SyncFilesToConnectionProtocolResponse]{
		streamer: syncClient,
		send: func(data []byte) error {
			if err := syncClient.Send(&graftv1.SyncFilesToConnectionProtocolRequest{Data: data}); err != nil {
				return errors.Wrap(err)
			}

			return nil
		},
		close: func() error {
			if err := syncClient.CloseSend(); err != nil {
				return errors.Wrap(err)
			}

			return nil
		},
	}
}

// newMutagenSyncStreamServerWrapper is used to handle the remote to local daemon part of the mutagen file sync protocol.
// It simply wraps the binary data from both sides in proto messages.
func newMutagenSyncStreamServerWrapper(
	syncServer graftv1.GraftService_SyncFilesToConnectionProtocolServer,
) *mutagenSyncStreamWrapper[*graftv1.SyncFilesToConnectionProtocolRequest] {
	return &mutagenSyncStreamWrapper[*graftv1.SyncFilesToConnectionProtocolRequest]{
		streamer: syncServer,
		send: func(data []byte) error {
			if err := syncServer.Send(&graftv1.SyncFilesToConnectionProtocolResponse{Data: data}); err != nil {
				return errors.Wrap(err)
			}

			return nil
		},
		close: func() error {
			return nil
		},
	}
}

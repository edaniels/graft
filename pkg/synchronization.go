package graft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/endpoint/remote"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// graftSyncNamePrefix tags every sync session we create so the orphan reaper
// can tell ours apart from any a user made with the standalone mutagen CLI.
const graftSyncNamePrefix = "graft-"

// syncSessionName returns a deterministic name for the intent's sync session.
// Same (connName, intent) always hashes the same, so on restart we can find
// and resume the paused session instead of creating a fresh one.
//
// Mutagen names allow only letters/numbers/dashes (selection.EnsureNameValid),
// so paths can't go in directly - we hash and hex-encode.
func syncSessionName(connName string, intent SynchronizationIntent) string {
	h := sha256.New()
	// Null bytes separate fields so ("a", "b/c") and ("a/b", "c") hash differently.
	h.Write([]byte(connName))
	h.Write([]byte{0})
	h.Write([]byte(filepath.Clean(intent.FromLocal)))
	h.Write([]byte{0})
	h.Write([]byte(filepath.Clean(intent.ToRemote)))

	return graftSyncNamePrefix + hex.EncodeToString(h.Sum(nil)[:10])
}

// findExistingSessionByName scans loaded sessions for a Name match, returning
// the session's identifier, paused state, and the ignore patterns it was
// created with. Returns ("", false, nil, nil) when not found.
func findExistingSessionByName(
	ctx context.Context,
	mgr *synchronization.Manager,
	name string,
) (string, bool, []string, error) {
	_, states, err := mgr.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		return "", false, nil, errors.Wrap(err)
	}

	for _, state := range states {
		sess := state.GetSession()
		if sess.GetName() == name {
			return sess.GetIdentifier(), sess.GetPaused(), sess.GetConfiguration().GetIgnores(), nil
		}
	}

	return "", false, nil, nil
}

// isGraftSyncName reports whether a mutagen session name was created by graft.
// The orphan reaper uses this to skip sessions created via the standalone
// mutagen CLI against the same data directory.
func isGraftSyncName(name string) bool {
	return strings.HasPrefix(name, graftSyncNamePrefix)
}

// syncProtoNum is the key under which graft registers its mutagen
// synchronization.ProtocolHandler, and the Protocol value embedded in every
// beta URL graft constructs. We claim Protocol_Docker because mutagen's
// URL.EnsureValid only accepts the three built-in protocols, and graft
// doesn't import mutagen's docker protocol package so the slot is free.
// Fixed so persisted beta URLs stay dispatchable after a daemon restart.
const syncProtoNum = int(urlpkg.Protocol_Docker)

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
	} else if url.GetProtocol() != urlpkg.Protocol(syncProtoNum) {
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

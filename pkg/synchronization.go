package graft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
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
// the session's identifier, paused state, and the session and beta endpoint
// configurations it was created with. Returns ("", false, nil, nil) when
// not found.
func findExistingSessionByName(
	ctx context.Context,
	mgr *synchronization.Manager,
	name string,
) (string, bool, *synchronization.Configuration, *synchronization.Configuration, error) {
	_, states, err := mgr.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		return "", false, nil, nil, errors.Wrap(err)
	}

	for _, state := range states {
		sess := state.GetSession()
		if sess.GetName() == name {
			return sess.GetIdentifier(), sess.GetPaused(),
				sess.GetConfiguration(), sess.GetConfigurationBeta(), nil
		}
	}

	return "", false, nil, nil, nil
}

// syncSessionConfigStale reports whether an existing session's recorded
// configurations diverge from the desired ones in any field graft manages.
// Mutagen sessions can't be reconfigured after creation, so a stale session
// must be terminated and recreated for the desired configuration to apply.
func syncSessionConfigStale(existingMain, existingBeta, desiredMain, desiredBeta *synchronization.Configuration) bool {
	return !slices.Equal(existingMain.GetIgnores(), desiredMain.GetIgnores()) ||
		existingBeta.GetDefaultFileMode() != desiredBeta.GetDefaultFileMode() ||
		existingBeta.GetDefaultDirectoryMode() != desiredBeta.GetDefaultDirectoryMode()
}

// Remote files and directories in a synced working tree are created with
// these modes instead of mutagen's portable-mode defaults (0600/0700), which
// break remote consumers running as other users, e.g. non-root container
// users reading docker bind mounts of a synced tree. They apply to the beta
// (remote) endpoint only; content the sync creates locally keeps mutagen's
// private defaults. Mutagen propagates the source's executable bit on top of
// the file base, so executable files land 0755. Per-file source modes are not
// otherwise mirrored (mutagen doesn't track them): locally-private 0600 files
// land at the tree's file mode like everything else.
const (
	syncDefaultFileMode      = filesystem.Mode(0o644)
	syncDefaultDirectoryMode = filesystem.Mode(0o755)
)

// parseSyncMode parses a user-facing octal permission mode string ("644",
// "0644", "0o644").
func parseSyncMode(s string) (filesystem.Mode, error) {
	if s == "" {
		return 0, errors.New("empty permission mode")
	}

	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0o"), "0O")

	parsed, err := strconv.ParseUint(digits, 8, 32)
	if err != nil {
		return 0, errors.WrapPrefix(err, fmt.Sprintf("invalid octal permission mode %q", s))
	}

	return filesystem.Mode(parsed), nil
}

// validateSyncModes checks an intent's octal mode strings. Empty strings are
// valid and mean "use defaults". File modes must not carry executability
// bits; mutagen owns those (propagated from the source).
func validateSyncModes(fileMode, dirMode string) error {
	if fileMode != "" {
		parsed, err := parseSyncMode(fileMode)
		if err != nil {
			return errors.WrapPrefix(err, "defaultFileMode")
		}

		if err := core.EnsureDefaultFileModeValid(core.PermissionsMode_PermissionsModePortable, parsed); err != nil {
			return errors.WrapPrefix(err, "defaultFileMode")
		}
	}

	if dirMode != "" {
		parsed, err := parseSyncMode(dirMode)
		if err != nil {
			return errors.WrapPrefix(err, "defaultDirectoryMode")
		}

		if err := core.EnsureDefaultDirectoryModeValid(core.PermissionsMode_PermissionsModePortable, parsed); err != nil {
			return errors.WrapPrefix(err, "defaultDirectoryMode")
		}
	}

	return nil
}

// syncModesCompatible reports whether a desired intent's modes are satisfied
// by an active sync's modes. Empty desired modes mean "no opinion", so a bare
// graft sync does not reset modes configured elsewhere.
func syncModesCompatible(existingFileMode, existingDirMode, desiredFileMode, desiredDirMode string) bool {
	return (desiredFileMode == "" || desiredFileMode == existingFileMode) &&
		(desiredDirMode == "" || desiredDirMode == existingDirMode)
}

// syncIncludesCompatible reports whether a desired intent's syncInclude
// overrides are satisfied by an active sync's. Empty desired means "no
// opinion", so a bare graft sync does not drop includes configured elsewhere.
//
// Comparison is order-insensitive: includes are all appended as "!" negations
// and produce the same ignore decision regardless of order, so merely
// reordering them must not be seen as a change (which would needlessly
// recreate the session and discard its ancestor archive).
func syncIncludesCompatible(existing, desired []string) bool {
	if len(desired) == 0 {
		return true
	}

	if len(existing) != len(desired) {
		return false
	}

	e := slices.Clone(existing)
	d := slices.Clone(desired)

	slices.Sort(e)
	slices.Sort(d)

	return slices.Equal(e, d)
}

// syncModesConfiguration builds a beta endpoint configuration from an
// intent's explicit modes, falling back to the given defaults when unset
// (zero defaults leave the field unset, deferring to mutagen's own).
func syncModesConfiguration(
	intent SynchronizationIntent,
	defaultFile, defaultDir filesystem.Mode,
) (*synchronization.Configuration, error) {
	cfg := &synchronization.Configuration{
		DefaultFileMode:      uint32(defaultFile),
		DefaultDirectoryMode: uint32(defaultDir),
	}

	if intent.DefaultFileMode != "" {
		mode, err := parseSyncMode(intent.DefaultFileMode)
		if err != nil {
			return nil, errors.WrapPrefix(err, "defaultFileMode")
		}

		if err := core.EnsureDefaultFileModeValid(core.PermissionsMode_PermissionsModePortable, mode); err != nil {
			return nil, errors.WrapPrefix(err, "defaultFileMode")
		}

		cfg.DefaultFileMode = uint32(mode)
	}

	if intent.DefaultDirectoryMode != "" {
		mode, err := parseSyncMode(intent.DefaultDirectoryMode)
		if err != nil {
			return nil, errors.WrapPrefix(err, "defaultDirectoryMode")
		}

		if err := core.EnsureDefaultDirectoryModeValid(core.PermissionsMode_PermissionsModePortable, mode); err != nil {
			return nil, errors.WrapPrefix(err, "defaultDirectoryMode")
		}

		cfg.DefaultDirectoryMode = uint32(mode)
	}

	return cfg, nil
}

// makeSyncRootCommand returns the shell command that ensures a sync root
// exists on the remote, pinning roots graft creates to the session's
// directory mode rather than the remote umask. Pre-existing roots are left
// untouched: they may deliberately carry other modes (a 0700 home directory,
// a setgid shared tree) or be owned by another user, where a chmod would
// fail the whole establishment. The chmod applies only to the root itself;
// intermediate directories mkdir -p creates keep the remote umask.
func makeSyncRootCommand(remotePath string, dirMode uint32) string {
	return fmt.Sprintf("test -d %[1]s || (mkdir -p %[1]s && chmod %[2]o %[1]s)", remotePath, dirMode)
}

// syncBetaConfiguration returns the beta (remote) endpoint configuration for
// a working-tree session: world-readable 0644/0755 unless the intent says
// otherwise.
func syncBetaConfiguration(intent SynchronizationIntent) (*synchronization.Configuration, error) {
	return syncModesConfiguration(intent, syncDefaultFileMode, syncDefaultDirectoryMode)
}

// gitReplicaBetaConfiguration returns the beta endpoint configuration for a
// .git replica session. Unlike the working tree, the replica defaults to
// mutagen's private 0600/0700: .git contents (credentials embedded in
// .git/config, full history in objects) are only needed by the remote user's
// own git commands, which run as the file owner. Explicit intent modes still
// apply, for the rare remote consumer that must read .git.
func gitReplicaBetaConfiguration(intent SynchronizationIntent) (*synchronization.Configuration, error) {
	return syncModesConfiguration(intent, 0, 0)
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
	// SyncGit enables a secondary one-way replica of FromLocal's .git
	// directory to the remote; see gitReplicaIntent.
	SyncGit bool
	// SyncInclude are gitignore-style patterns for content that must sync even
	// though .gitignore excludes it (e.g. generated protobufs): gitignored for
	// git, still synced by graft. Applied as trailing "!" negations over the
	// gitignore-derived ignores; see applySyncIncludes.
	SyncInclude []string
	// DefaultFileMode and DefaultDirectoryMode are octal permission mode
	// strings for content the sync writes on the remote; see
	// [SynchronizationIntentConfig] for semantics. Empty means defaults.
	DefaultFileMode      string
	DefaultDirectoryMode string
}

// gitReplicaIntent derives the .git replica session's endpoints from a parent
// intent. The distinct paths give the replica session a distinct
// syncSessionName, so the parent and replica sessions never collide.
func gitReplicaIntent(intent SynchronizationIntent) SynchronizationIntent {
	return SynchronizationIntent{
		FromLocal: filepath.Join(intent.FromLocal, gitDirName),
		ToRemote:  intent.ToRemote + "/" + gitDirName,
	}
}

// gitDirName is the name of git's metadata directory under a repository root.
const gitDirName = ".git"

// gitReplicaIgnores are the Mutagen-syntax ignore patterns for .git replica
// sessions. Git's transient lock and temp files must never propagate: a
// replicated index.lock would make remote git refuse to run until the next
// flush removed it. Changing this list recreates existing replica sessions
// via the ignore-drift check in EstablishSynchronization.
var gitReplicaIgnores = []string{
	"*.lock",
	"objects/**/tmp_*",
	"gc.pid",
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

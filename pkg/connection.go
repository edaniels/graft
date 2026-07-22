package graft

import (
	"bytes"
	"context"
	"encoding/gob"
	"hash/fnv"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"golang.org/x/crypto/ssh/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
	"github.com/edaniels/graft/pkg/embedded"
)

// various paths and commands.
const (
	defaultShellPath = "/bin/sh"
	homeDirCmd       = "printenv HOME"
	unameOSCmd       = "uname -s"
	unameArchCmd     = "uname -m"
)

type activeSync struct {
	destination string
	closeFunc   func()
	// syncGit records whether the intent enables the .git replica session.
	// gitCloseFunc pauses that session; it is nil when the replica is
	// disabled.
	syncGit      bool
	gitCloseFunc func()
	// defaultFileMode/defaultDirectoryMode record the intent's explicit
	// remote mode overrides ("" when graft's defaults apply); used to detect
	// configuration changes that require recreating the sessions.
	defaultFileMode      string
	defaultDirectoryMode string
	// syncInclude records the intent's syncInclude override patterns; used to
	// detect changes that require recreating the session (they alter the
	// assembled ignore list).
	syncInclude []string
}

// inheritModesInto fills an intent's empty modes from the active sync's
// recorded modes. Empty means "no opinion": a bare graft sync (or a .git
// replica flip) must not reset modes configured elsewhere.
func (s activeSync) inheritModesInto(intent *SynchronizationIntent) {
	if intent.DefaultFileMode == "" {
		intent.DefaultFileMode = s.defaultFileMode
	}

	if intent.DefaultDirectoryMode == "" {
		intent.DefaultDirectoryMode = s.defaultDirectoryMode
	}
}

// Connection is a lightweight per-connection wrapper around a shared remoteDaemon.
// It holds per-connection metadata (name, roots, forward intents, synchronizations)
// and derives its state from the daemon.
type Connection struct {
	daemon *remoteDaemon

	name       string
	localRoot  string
	remoteRoot string
	background bool

	mu               sync.Mutex
	stateHash        uint32
	closed           bool
	fwdList          []ForwardCommandIntent
	synchronizations map[string]activeSync
}

// newConnection creates a connection backed by the given daemon.
func newConnection(daemon *remoteDaemon, name, localRoot, remoteRoot string, background bool) *Connection {
	return &Connection{
		daemon:           daemon,
		name:             name,
		localRoot:        localRoot,
		remoteRoot:       remoteRoot,
		background:       background,
		synchronizations: map[string]activeSync{},
	}
}

// Background returns whether this is a background connection excluded from CWD-based auto-selection.
func (conn *Connection) Background() bool {
	return conn.background
}

// RunCommand runs a command on the remote daemon.
func (conn *Connection) RunCommand(
	ctx context.Context,
	cwd string,
	shell bool,
	command string,
	arguments []string,
	extraEnv []string,
	sudo bool,
	allocatePty bool,
	redirectStdout, redirectStderr bool,
) (RunningCommand, error) {
	cc, err := conn.daemon.lockedRemoteClientConn()
	if err != nil {
		return nil, err
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	runClient, err := remClient.RunCommand(ctx)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	sendErr := runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Start{
			Start: &graftv1.StartCommand{
				Cwd:            cwd,
				Shell:          shell,
				ExactCommand:   true,
				Command:        command,
				Arguments:      arguments,
				ExtraEnv:       extraEnv,
				Sudo:           sudo,
				AllocatePty:    allocatePty,
				RedirectStdout: redirectStdout,
				RedirectStderr: redirectStderr,
			},
		},
	})
	if sendErr != nil {
		return nil, errors.Wrap(sendErr)
	}

	// Wait for the remote daemon to confirm the command is running.
	// This ensures the local daemon's own Started message truthfully means
	// the command is running end-to-end.
	resp, err := runClient.Recv()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	if _, ok := resp.GetData().(*graftv1.RunCommandResponse_Started); !ok {
		return nil, errors.New("expected CommandStarted response from remote daemon")
	}

	return NewRemoteRunningCommand(runClient), nil
}

func (conn *Connection) ForwardSSHAgent(
	ctx context.Context,
) error {
	cc, err := conn.daemon.lockedRemoteClientConn()
	if err != nil {
		return err
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	sshAgentClient, err := remClient.ForwardSSHAgent(ctx)
	if err != nil {
		return errors.Wrap(err)
	}

	if sendErr := sshAgentClient.Send(&graftv1.ForwardSSHAgentRequest{}); sendErr != nil {
		return errors.Wrap(sendErr)
	}

	var dialer net.Dialer

	sock, err := dialer.DialContext(ctx, "unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		slog.ErrorContext(ctx, "error dialing ssh auth sock", "error", err)

		return nil
	}
	defer sock.Close()

	sockAgent := agent.NewClient(sock)

	localToRemoteConn := newForwardSSHAgentStreamClientWrapper(sshAgentClient)

	return errors.Wrap(agent.ServeAgent(sockAgent, localToRemoteConn))
}

type forwardSSHAgentStreamer[ReadT binaryDataMessage] interface {
	Recv() (ReadT, error)
}

type forwardSSHAgentStreamWrapper[ReadT binaryDataMessage] struct {
	streamer forwardSSHAgentStreamer[ReadT]
	buf      bytes.Buffer
	send     func(data []byte) error
	close    func() error
}

func (w *forwardSSHAgentStreamWrapper[ReadT]) Read(p []byte) (int, error) {
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

func (w *forwardSSHAgentStreamWrapper[ReadT]) Write(p []byte) (int, error) {
	err := w.send(p)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (w *forwardSSHAgentStreamWrapper[ReadT]) Close() error {
	return w.close()
}

func newForwardSSHAgentStreamClientWrapper(
	syncClient graftv1.GraftService_ForwardSSHAgentClient,
) *forwardSSHAgentStreamWrapper[*graftv1.ForwardSSHAgentResponse] {
	return &forwardSSHAgentStreamWrapper[*graftv1.ForwardSSHAgentResponse]{
		streamer: syncClient,
		send: func(data []byte) error {
			if err := syncClient.Send(&graftv1.ForwardSSHAgentRequest{Data: data}); err != nil {
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

func newForwardSSHAgentStreamServerWrapper(
	syncServer graftv1.GraftService_ForwardSSHAgentServer,
) *forwardSSHAgentStreamWrapper[*graftv1.ForwardSSHAgentRequest] {
	return &forwardSSHAgentStreamWrapper[*graftv1.ForwardSSHAgentRequest]{
		streamer: syncServer,
		send: func(data []byte) error {
			if err := syncServer.Send(&graftv1.ForwardSSHAgentResponse{Data: data}); err != nil {
				return errors.Wrap(err)
			}

			return nil
		},
		close: func() error {
			return nil
		},
	}
}

// ForwardIntents returns the forward intents for this connection.
func (conn *Connection) ForwardIntents() []ForwardCommandIntent {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return slices.Clone(conn.fwdList)
}

// UpdateForwardCommands updates the commands a user wishes to forward.
func (conn *Connection) UpdateForwardCommands(commands []ForwardCommandIntent) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	conn.fwdList = append(conn.fwdList, commands...)
}

// RemoveForwardCommands removes the named commands from the forward list.
func (conn *Connection) RemoveForwardCommands(commands []string) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	toRemove := make(map[string]struct{}, len(commands))
	for _, cmd := range commands {
		toRemove[cmd] = struct{}{}
	}

	conn.fwdList = slices.DeleteFunc(conn.fwdList, func(intent ForwardCommandIntent) bool {
		_, remove := toRemove[intent.Name]

		return remove
	})
}

func (conn *Connection) Name() string {
	return conn.name
}

func (conn *Connection) LocalRoot() string {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.localRoot
}

func (conn *Connection) RemoteRoot() string {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.remoteRoot
}

func (conn *Connection) Roots() (string, string) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.localRoot, conn.remoteRoot
}

func (conn *Connection) SetRoots(localRoot, remoteRoot string) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if len(conn.synchronizations) > 0 {
		return errors.New(
			"cannot update roots on a connection with active synchronization; disconnect and reconnect with new roots",
		)
	}

	if localRoot != "" {
		conn.localRoot = localRoot
	}

	if remoteRoot != "" {
		conn.remoteRoot = remoteRoot
	}

	return nil
}

// lockedDaemon returns the connection's daemon, safe for concurrent access.
func (conn *Connection) lockedDaemon() *remoteDaemon {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	return conn.daemon
}

// updateDaemon replaces the connection's daemon pointer (e.g. after supersede).
func (conn *Connection) updateDaemon(d *remoteDaemon) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	conn.daemon = d
}

// State returns the connection state. If this connection is closed, it returns Closed.
// Otherwise it returns the daemon's state.
func (conn *Connection) State() (ConnectionState, string) {
	conn.mu.Lock()
	closed := conn.closed
	d := conn.daemon
	conn.mu.Unlock()

	if closed {
		return ConnectionStateClosed, ""
	}

	return d.State()
}

func (conn *Connection) HomeDir() string {
	return conn.daemon.HomeDir()
}

// DumpLogs delegates to the daemon.
func (conn *Connection) DumpLogs(ctx context.Context) (string, string, error) {
	return conn.daemon.DumpLogs(ctx)
}

// Synchronizations returns the active synchronizations for this connection.
func (conn *Connection) Synchronizations() []SynchronizationIntent {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	syncs := make([]SynchronizationIntent, 0, len(conn.synchronizations))
	for from, to := range conn.synchronizations {
		syncs = append(syncs, SynchronizationIntent{
			FromLocal:            from,
			ToRemote:             to.destination,
			SyncGit:              to.syncGit,
			SyncInclude:          to.syncInclude,
			DefaultFileMode:      to.defaultFileMode,
			DefaultDirectoryMode: to.defaultDirectoryMode,
		})
	}

	return syncs
}

// EstablishSynchronization sets up bidi file sync. Idempotent: if a sync with the
// exact same (FromLocal, ToRemote) intent is already active, it is a no-op. If a
// sync exists for the same FromLocal but with a different destination, an error is
// returned.
//
// It returns any syncInclude patterns that are shadowed by a directory-level
// ignore and so will not take effect (see shadowedSyncIncludes); callers should
// surface these to the user. The slice is non-nil only when a session is
// actually created or recreated.
func (conn *Connection) EstablishSynchronization(
	ctx context.Context,
	syncIntent SynchronizationIntent,
	syncManager *synchronization.Manager,
) ([]string, error) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if existing, ok := conn.synchronizations[syncIntent.FromLocal]; ok {
		existing.inheritModesInto(&syncIntent)

		// Empty means "no opinion": a bare graft sync must not drop includes
		// configured elsewhere (mirrors mode inheritance above).
		if len(syncIntent.SyncInclude) == 0 {
			syncIntent.SyncInclude = existing.syncInclude
		}
	}

	// Fast path: when reconciling from config (which stores already-resolved paths),
	// the intent matches an active sync exactly. Skip without doing any remote I/O.
	if existing, ok := conn.synchronizations[syncIntent.FromLocal]; ok &&
		existing.destination == syncIntent.ToRemote && existing.syncGit == syncIntent.SyncGit &&
		syncModesCompatible(existing.defaultFileMode, existing.defaultDirectoryMode,
			syncIntent.DefaultFileMode, syncIntent.DefaultDirectoryMode) &&
		syncIncludesCompatible(existing.syncInclude, syncIntent.SyncInclude) {
		return nil, nil
	}

	resolvedPath, resolveErr := conn.daemon.Connector().RunOneShotCommand(ctx, "echo "+syncIntent.ToRemote)
	if resolveErr != nil {
		return nil, errors.WrapPrefix(resolveErr, "error resolving sync to remote path")
	}

	syncIntent.ToRemote = strings.TrimSpace(strings.TrimSuffix(resolvedPath, "\n"))

	// Re-check after resolution: an unresolved intent (e.g., "~/foo") may now match
	// an already-active resolved entry.
	if existing, ok := conn.synchronizations[syncIntent.FromLocal]; ok {
		if existing.destination != syncIntent.ToRemote {
			return nil, errors.Errorf(
				"synchronization for %q already exists with destination %q (cannot override to %q)",
				syncIntent.FromLocal, existing.destination, syncIntent.ToRemote,
			)
		}

		if syncModesCompatible(existing.defaultFileMode, existing.defaultDirectoryMode,
			syncIntent.DefaultFileMode, syncIntent.DefaultDirectoryMode) &&
			syncIncludesCompatible(existing.syncInclude, syncIntent.SyncInclude) {
			if existing.syncGit == syncIntent.SyncGit {
				return nil, nil
			}

			return nil, conn.applyGitReplicaFlip(ctx, syncManager, syncIntent, existing)
		}

		// Explicit mode or syncInclude changes fall through: establishSession
		// terminates the now-stale mutagen sessions and creates fresh ones (the
		// new includes alter the ignore list), and the map entry is replaced below.
	}

	// Assembled here, past the no-op returns above, so the shadowed-include
	// check runs only when a session is genuinely (re)created rather than on
	// every reconcile tick.
	ignores := parseGitignoreToMutagenIgnores(syncIntent.FromLocal)
	ignores = applySyncIncludes(ignores, syncIntent.SyncInclude)
	shadowed := shadowedSyncIncludes(ignores, syncIntent.SyncInclude)

	betaConfig, betaErr := syncBetaConfiguration(syncIntent)
	if betaErr != nil {
		return nil, errors.WrapPrefix(betaErr, "invalid sync permission modes")
	}

	// The sync root is created by graft, not mutagen, so its mode must be
	// pinned to the session's directory mode rather than inherited from the
	// remote umask: a umask-derived 0700 root would block traversal into an
	// otherwise world-readable tree.
	mkdirCmd := makeSyncRootCommand(syncIntent.ToRemote, betaConfig.GetDefaultDirectoryMode())

	if _, mkdirErr := conn.daemon.Connector().RunOneShotCommand(ctx, mkdirCmd); mkdirErr != nil {
		return nil, errors.WrapPrefix(mkdirErr, "error making directory for sync to remote path")
	}

	cc, err := conn.daemon.lockedRemoteClientConn()
	if err != nil {
		return nil, errors.New("connection is not available")
	}

	sessionID, err := conn.establishSession(ctx, cc, syncManager,
		syncSessionName(conn.Name(), syncIntent),
		syncIntent.FromLocal, syncIntent.ToRemote,
		&synchronization.Configuration{
			SynchronizationMode: core.SynchronizationMode_SynchronizationModeTwoWayResolved,
			IgnoreSyntax:        ignore.Syntax_SyntaxMutagen,
			IgnoreVCSMode:       ignore.IgnoreVCSMode_IgnoreVCSModeIgnore,
			Ignores:             ignores,
		},
		betaConfig,
	)
	if err != nil {
		return nil, err
	}

	// The mode-change fall-through can be replacing an entry whose replica is
	// being dropped along with the mode change; pause it now rather than
	// leaving it replicating until the orphan reaper's next tick.
	if existing, ok := conn.synchronizations[syncIntent.FromLocal]; ok &&
		existing.gitCloseFunc != nil && !syncIntent.SyncGit {
		existing.gitCloseFunc()
	}

	// Record the primary session before attempting the replica so a replica
	// failure cannot leave the entry pointing at a terminated prior session;
	// the reconcile loop heals the missing replica via applyGitReplicaFlip.
	entry := activeSync{
		destination:          syncIntent.ToRemote,
		closeFunc:            pauseSessionFunc(syncManager, sessionID),
		defaultFileMode:      syncIntent.DefaultFileMode,
		defaultDirectoryMode: syncIntent.DefaultDirectoryMode,
		syncInclude:          syncIntent.SyncInclude,
	}
	conn.synchronizations[syncIntent.FromLocal] = entry

	if syncIntent.SyncGit {
		gitCloseFunc, gitErr := conn.establishGitReplica(ctx, cc, syncManager, syncIntent)
		if gitErr != nil {
			return nil, gitErr
		}

		entry.syncGit = true
		entry.gitCloseFunc = gitCloseFunc
		conn.synchronizations[syncIntent.FromLocal] = entry
	}

	return shadowed, nil
}

// applyGitReplicaFlip handles an intent whose endpoints match an active sync
// but whose SyncGit flag differs: only the .git replica session is
// established or wound down; the primary session is untouched.
func (conn *Connection) applyGitReplicaFlip(
	ctx context.Context,
	syncManager *synchronization.Manager,
	syncIntent SynchronizationIntent,
	existing activeSync,
) error {
	if syncIntent.SyncGit {
		cc, err := conn.daemon.lockedRemoteClientConn()
		if err != nil {
			return errors.New("connection is not available")
		}

		gitCloseFunc, err := conn.establishGitReplica(ctx, cc, syncManager, syncIntent)
		if err != nil {
			return err
		}

		existing.gitCloseFunc = gitCloseFunc
	} else {
		if existing.gitCloseFunc != nil {
			existing.gitCloseFunc()
		}

		// The orphan reaper terminates the now-unexpected replica session.
		existing.gitCloseFunc = nil
	}

	existing.syncGit = syncIntent.SyncGit
	conn.synchronizations[syncIntent.FromLocal] = existing

	return nil
}

// establishGitReplica sets up the one-way .git replica session for the
// intent, giving the remote a read-only git view: remote writes to .git are
// reverted on the next flush rather than syncing back. Returns a pause func
// for the session; when the replica is skipped because FromLocal has no
// .git directory, the returned func is a no-op.
func (conn *Connection) establishGitReplica(
	ctx context.Context,
	cc *grpc.ClientConn,
	syncManager *synchronization.Manager,
	syncIntent SynchronizationIntent,
) (func(), error) {
	gitIntent := gitReplicaIntent(syncIntent)

	// A missing .git means FromLocal isn't a repository root; a .git file is
	// a worktree/submodule gitdir pointer whose target is local-only, so
	// replicating it would produce a dangling repository on the remote.
	if info, statErr := os.Stat(gitIntent.FromLocal); statErr != nil || !info.IsDir() {
		slog.WarnContext(ctx, "skipping git replica sync; local .git is not a directory",
			"path", gitIntent.FromLocal)

		return func() {}, nil //nolint:nilerr // a non-directory .git skips the replica by design
	}

	replicaBetaConfig, betaErr := gitReplicaBetaConfiguration(syncIntent)
	if betaErr != nil {
		return nil, errors.WrapPrefix(betaErr, "invalid sync permission modes")
	}

	// An unset replica directory mode means mutagen's private 0700 default;
	// the graft-created replica root must match rather than inherit the
	// remote umask.
	replicaDirMode := replicaBetaConfig.GetDefaultDirectoryMode()
	if replicaDirMode == 0 {
		replicaDirMode = 0o700
	}

	mkdirCmd := makeSyncRootCommand(gitIntent.ToRemote, replicaDirMode)

	if _, mkdirErr := conn.daemon.Connector().RunOneShotCommand(ctx, mkdirCmd); mkdirErr != nil {
		return nil, errors.WrapPrefix(mkdirErr, "error making directory for git replica remote path")
	}

	sessionID, err := conn.establishSession(ctx, cc, syncManager,
		syncSessionName(conn.Name(), gitIntent),
		gitIntent.FromLocal, gitIntent.ToRemote,
		&synchronization.Configuration{
			SynchronizationMode: core.SynchronizationMode_SynchronizationModeOneWayReplica,
			IgnoreSyntax:        ignore.Syntax_SyntaxMutagen,
			// The session root is itself a VCS directory; ignoring VCS
			// content would ignore everything.
			IgnoreVCSMode: ignore.IgnoreVCSMode_IgnoreVCSModePropagate,
			Ignores:       gitReplicaIgnores,
		},
		replicaBetaConfig,
	)
	if err != nil {
		return nil, err
	}

	return pauseSessionFunc(syncManager, sessionID), nil
}

// establishSession resumes a paused session matching sessionName, or creates
// a fresh one from alphaPath to betaPath with the given session and beta
// endpoint configurations. Resuming keeps the ancestor archive, which is what
// stops locally-deleted files from coming back on the next sync.
func (conn *Connection) establishSession(
	ctx context.Context,
	cc *grpc.ClientConn,
	syncManager *synchronization.Manager,
	sessionName string,
	alphaPath, betaPath string,
	config *synchronization.Configuration,
	betaConfig *synchronization.Configuration,
) (string, error) {
	existingID, existingPaused, existingConfig, existingBetaConfig, lookupErr := findExistingSessionByName(
		ctx, syncManager, sessionName)
	if lookupErr != nil {
		return "", errors.WrapPrefix(lookupErr, "error looking up existing sync session")
	}

	// A session's configuration is fixed at creation (mutagen has no
	// reconfigure API), so a session whose recorded configuration no longer
	// matches the desired one must be terminated and recreated. This discards
	// the ancestor archive; remote files whose local counterparts were
	// deleted may reappear locally on the next sync.
	if existingID != "" && syncSessionConfigStale(existingConfig, existingBetaConfig, config, betaConfig) {
		slog.WarnContext(ctx, "session configuration changed; recreating sync session",
			"session_id", existingID, "name", sessionName)

		if termErr := syncManager.Terminate(
			ctx, &selection.Selection{Specifications: []string{existingID}}, "",
		); termErr != nil {
			return "", errors.WrapPrefix(termErr, "error terminating sync session with stale configuration")
		}

		existingID = ""
	}

	if existingID != "" {
		if existingPaused {
			if resumeErr := syncManager.Resume(
				ContextWithConnRemoteClientConn(ctx, cc),
				&selection.Selection{Specifications: []string{existingID}},
				"",
			); resumeErr != nil {
				return "", errors.WrapPrefix(resumeErr, "error resuming existing sync")
			}
		}

		return existingID, nil
	}

	sessionID, err := syncManager.Create(
		ContextWithConnRemoteClientConn(ctx, cc),
		&urlpkg.URL{
			Protocol: urlpkg.Protocol_Local,
			Path:     alphaPath,
		},
		&urlpkg.URL{
			Protocol: urlpkg.Protocol(syncProtoNum),
			Host:     conn.Name(),
			Path:     betaPath,
		},
		config,
		&synchronization.Configuration{},
		betaConfig,
		sessionName,
		nil,
		false,
		"huh????",
	)
	if err != nil {
		return "", errors.Wrap(err)
	}

	return sessionID, nil
}

// pauseSessionFunc returns a func that pauses the given session. Pause, not
// Terminate: the ancestor archive stays on disk. Termination only happens in
// the orphan reaper.
func pauseSessionFunc(syncManager *synchronization.Manager, sessionID string) func() {
	return func() {
		if err := syncManager.Pause(context.Background(), &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
			slog.Error("error pausing sync", "session_id", sessionID, "error", err)
		}
	}
}

// MatchCWD transforms a local directory into a remote directory.
func (conn *Connection) MatchCWD(from string) (string, bool) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	localRoot := conn.localRoot
	remoteRoot := conn.remoteRoot

	if localRoot != "" && remoteRoot != "" {
		if after, ok := hasPathPrefix(from, localRoot); ok {
			return filepath.Join(remoteRoot, after), true
		}
	}

	for candFrom, activeSync := range conn.synchronizations {
		if after, ok := hasPathPrefix(from, candFrom); ok {
			return filepath.Join(activeSync.destination, after), true
		}
	}

	slog.Debug("no cwd match found", "directory", from)

	return "", false
}

// remoteConnToGRPCClientConn returns a gRPC connection from a pre-established net.Conn.
func remoteConnToGRPCClientConn(remoteConn net.Conn) (*grpc.ClientConn, error) {
	client, err := grpc.NewClient("passthrough://zzz",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return remoteConn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, errors.WrapPrefix(err, "unlikely")
	}

	return client, nil
}

// Close terminates any active synchronizations and marks this connection
// as closed. The daemon is NOT closed here; the ConnectionManager handles
// daemon lifecycle via reference counting.
func (conn *Connection) Close() error {
	conn.mu.Lock()
	syncs := conn.synchronizations
	conn.synchronizations = nil
	conn.closed = true
	conn.mu.Unlock()

	for _, s := range syncs {
		s.closeFunc()

		if s.gitCloseFunc != nil {
			s.gitCloseFunc()
		}
	}

	return nil
}

// Destroy closes the connection. Daemon destruction is handled by ConnectionManager.
func (conn *Connection) Destroy(_ context.Context) error {
	return conn.Close()
}

// Hash returns a hash representing the current connection status.
func (conn *Connection) Hash(resp *graftv1.StatusResponse) (uint32, bool) {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	d := conn.daemon
	state, _ := d.State()

	hasher := fnv.New32()

	enc := gob.NewEncoder(hasher)
	if err := enc.Encode(state); err != nil {
		panic(err)
	}

	if resp != nil {
		if err := enc.Encode(resp.GetHealthy()); err != nil {
			panic(err)
		}

		if err := enc.Encode(resp.GetVersionInfo()); err != nil {
			panic(err)
		}

		for _, log := range resp.GetRecentLogs() {
			if err := enc.Encode(log); err != nil {
				panic(err)
			}
		}
	}

	newHash := hasher.Sum32()
	changed := newHash != conn.stateHash
	conn.stateHash = newHash

	return newHash, changed
}

func (conn *Connection) StateFields() []any {
	return conn.lockedDaemon().StateFields()
}

// daemonBinName returns the architecture specific daemon name to use for a connection.
func daemonBinName(os, arch string) string {
	return BinaryName(os, arch)
}

// daemonBinPath returns the architecture specific daemon file path to use for a connection.
// For release builds with embedded binaries, it extracts the binary to a temp file.
// For development builds, it compiles the binary locally.
func daemonBinPath(ctx context.Context, osName, archName string) (string, error) {
	// Try embedded binary first (release builds)
	binName := BinaryName(osName, archName)

	binData, err := embedded.GetBinary(binName)
	if err != nil {
		// Distinguish between "not embedded" (expected, fall back) and real errors (fail)
		var notEmbedded embedded.NotEmbeddedError
		if !errors.As(err, &notEmbedded) {
			return "", errors.WrapPrefix(err, "error getting embedded binary")
		}
		// NotEmbeddedError: fall through to local compilation
	}

	if binData != nil {
		slog.DebugContext(ctx, "extracting embedded binary", "os", osName, "arch", archName)

		return extractEmbeddedBinary(osName, archName, binData)
	}

	// Fall back to local compilation (development builds)
	slog.DebugContext(ctx, "building binary locally", "os", osName, "arch", archName)

	return buildLocalBinary(ctx, osName, archName)
}

// extractEmbeddedBinary writes the embedded binary data to a version-specific cache directory and returns its path.
// Binaries are cached at ~/.cache/graft/binaries/{version}/graft-{os}-{arch} to avoid re-extracting on each connection.
func extractEmbeddedBinary(osName, archName string, binData []byte) (string, error) {
	cacheDir, err := DaemonCacheDir()
	if err != nil {
		return "", errors.WrapPrefix(err, "error getting cache directory")
	}

	// Use version string for cache directory to ensure correct binary per version
	version := VersionString()
	versionedCacheDir := filepath.Join(cacheDir, version)

	binName := daemonBinName(osName, archName)
	binPath := filepath.Join(versionedCacheDir, binName)

	// Check if already extracted (existence is sufficient since path includes version)
	if _, statErr := os.Stat(binPath); statErr == nil {
		return binPath, nil
	}

	if err := os.MkdirAll(versionedCacheDir, DirPerms); err != nil {
		return "", errors.WrapPrefix(err, "error creating cache directory")
	}

	// Write to temp file then rename for atomicity; include PID to avoid races
	tmpPath := binPath + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmpPath, binData, ExecFilePerms); err != nil {
		return "", errors.WrapPrefix(err, "error writing binary")
	}

	if err := os.Rename(tmpPath, binPath); err != nil {
		os.Remove(tmpPath)

		return "", errors.WrapPrefix(err, "error renaming binary")
	}

	return binPath, nil
}

// buildLocalBinary compiles the daemon binary for the given OS/arch (development only).
func buildLocalBinary(ctx context.Context, osName, archName string) (string, error) {
	if !VersionIsInSourceTree() {
		return "", errors.New("no embedded binary available and not in source tree")
	}

	graftBinName := daemonBinName(osName, archName)
	//nolint:dogsled
	_, connFilePath, _, _ := runtime.Caller(0)

	connFileDirPath, err := filepath.Abs(filepath.Dir(connFilePath))
	if err != nil {
		return "", errors.Wrap(err)
	}

	repoRoot := filepath.Join(connFileDirPath, "..")
	binPath := filepath.Join(repoRoot, "bin", graftBinName)

	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/graft")
	cmd.Dir = repoRoot
	cmd.Env = slices.DeleteFunc(os.Environ(), func(v string) bool {
		return strings.HasPrefix(v, "CGO_ENABLED=") || strings.HasPrefix(v, "GOOS=") || strings.HasPrefix(v, "GOARCH=")
	})
	cmd.Env = append(cmd.Env,
		"CGO_ENABLED=0",
		"GOOS="+osName,
		"GOARCH="+archName,
	)

	if err := cmd.Run(); err != nil {
		return "", errors.Wrap(err)
	}

	return binPath, nil
}

func (conn *Connection) AvailableCommands() []string {
	global, byDir := conn.daemon.AvailableCommands()

	collected := map[string]struct{}{}
	// TODO(erd): we should make the env provider actually dictate the ordering of commands so that we don't have
	// different semantics for how PATHs are searched
	for _, cmd := range byDir[conn.RemoteRoot()] {
		collected[cmd] = struct{}{}
	}

	for _, cmd := range global {
		if _, ok := collected[cmd]; ok {
			continue
		}

		collected[cmd] = struct{}{}
	}

	return slices.Collect(maps.Keys(collected))
}

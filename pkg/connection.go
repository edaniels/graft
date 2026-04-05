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

	"github.com/denormal/go-gitignore"
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
		syncs = append(syncs, SynchronizationIntent{FromLocal: from, ToRemote: to.destination})
	}

	return syncs
}

// EstablishSynchronization sets up bidi file sync.
func (conn *Connection) EstablishSynchronization(
	ctx context.Context,
	syncIntent SynchronizationIntent,
	syncManager *synchronization.Manager,
	syncProtoNum int,
) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	var ignores []string

	rd, ignoreErr := os.ReadFile(filepath.Join(syncIntent.FromLocal, ".gitignore"))
	if ignoreErr == nil {
		parser := gitignore.NewParser(bytes.NewReader(rd), func(_ gitignore.Error) bool {
			return true
		})

		patterns := parser.Parse()
		for _, pattern := range patterns {
			ignores = append(ignores, pattern.String())
		}
	}

	resolvedPath, resolveErr := conn.daemon.Connector().RunOneShotCommand(ctx, "echo "+syncIntent.ToRemote)
	if resolveErr != nil {
		return errors.WrapPrefix(resolveErr, "error resolving sync to remote path")
	}

	syncIntent.ToRemote = strings.TrimSpace(strings.TrimSuffix(resolvedPath, "\n"))

	_, mkdirErr := conn.daemon.Connector().RunOneShotCommand(ctx, "mkdir -p "+syncIntent.ToRemote)
	if mkdirErr != nil {
		return errors.WrapPrefix(mkdirErr, "error making directory for sync to remote path")
	}

	cc, err := conn.daemon.lockedRemoteClientConn()
	if err != nil {
		return errors.New("connection is not available")
	}

	sessionID, err := syncManager.Create(
		ContextWithConnRemoteClientConn(ctx, cc),
		&urlpkg.URL{
			Protocol: urlpkg.Protocol_Local,
			Path:     syncIntent.FromLocal,
		},
		&urlpkg.URL{
			Protocol: urlpkg.Protocol(syncProtoNum), //nolint:gosec // overflow okay
			Host:     conn.Name(),
			Path:     syncIntent.ToRemote,
		},
		&synchronization.Configuration{
			SynchronizationMode: core.SynchronizationMode_SynchronizationModeTwoWayResolved,
			IgnoreSyntax:        ignore.Syntax_SyntaxMutagen,
			IgnoreVCSMode:       ignore.IgnoreVCSMode_IgnoreVCSModeIgnore,
			Ignores:             ignores,
		},
		&synchronization.Configuration{},
		&synchronization.Configuration{},
		filepath.Base(syncIntent.ToRemote),
		nil,
		false,
		"huh????",
	)
	if err != nil {
		return errors.Wrap(err)
	}

	conn.synchronizations[syncIntent.FromLocal] = activeSync{
		syncIntent.ToRemote,
		func() {
			if err := syncManager.Terminate(context.Background(), &selection.Selection{Specifications: []string{sessionID}}, ""); err != nil {
				slog.ErrorContext(ctx, "error terminating sync", "session_id", sessionID, "error", err)
			}
		},
	}

	return nil
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

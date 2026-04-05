package graft

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// discoveryInfo holds the results of discovering the remote host's environment.
// All fields are derived from the remote host and are identical for all connections
// sharing the same remoteDaemon.
type discoveryInfo struct {
	OS               string
	Arch             string
	HomeDir          string
	RemoteSocketPath string
}

type activePortForward struct {
	remotePort     uint32
	localPort      uint32
	protocol       string
	listener       net.Listener       // for TCP
	cancel         context.CancelFunc // cancels listener accept loop (fwdCtx)
	relayCancel    context.CancelFunc // cancels relay goroutines (relayCtx); used during Close()
	conflict       bool
	conflictReason string
	explicit       bool // true if user-requested, false if auto-detected
}

// remoteDaemon is the first-class entity representing a connection to a remote daemon.
// It owns the transport (connector), gRPC connection, daemon lifecycle, health checking,
// reconnection, command discovery, and port forwarding. Connections are lightweight
// wrappers that reference a daemon for running commands and derive their state from
// the daemon's state.
type remoteDaemon struct {
	connector RemoteConnector
	logLevel  slog.Level

	//nolint:containedctx // long-lived context for monitoring goroutines; set by ConnectionManager
	runCtx context.Context

	mu                sync.Mutex
	state             ConnectionState
	stateReason       string
	closed            bool
	reconnecting      bool
	remoteConn        RemoteDaemonConnection
	homeDir           string
	remoteSocketPath  string
	cancelMonitor     func()
	cancelMonitorPort func()
	portForwards      map[string]*activePortForward
	explicitPorts     map[string]PortForwardSpec // keyed by portForwardKey(protocol, remotePort)

	remoteRoots       atomic.Pointer[[]string]
	availableCommands atomic.Pointer[[]string]
	activeWorkers     sync.WaitGroup

	installMu       sync.Mutex
	reinstalledOnce bool

	discoverMu    sync.Mutex
	discoveryDone bool
	discovery     discoveryInfo
	discoveryErr  error

	initDone     chan struct{} // closed when Initialize completes
	initErr      error         // result stored by the first Initialize caller
	supersededBy *remoteDaemon // set when an existing daemon already handles this destination

	mapKey   string // key in ConnectionManager.daemons; managed under connMgrMu
	refCount int    // managed by ConnectionManager under connMgrMu
}

// newRemoteDaemon creates a remoteDaemon that owns the given connector.
func newRemoteDaemon(connector RemoteConnector, logLevel slog.Level) *remoteDaemon {
	d := &remoteDaemon{
		connector:     connector,
		logLevel:      logLevel,
		portForwards:  map[string]*activePortForward{},
		explicitPorts: map[string]PortForwardSpec{},
	}
	d.availableCommands.Store(&[]string{})

	return d
}

// Connector returns the underlying transport connector.
func (d *remoteDaemon) Connector() RemoteConnector {
	return d.connector
}

// State returns the current daemon state and reason.
func (d *remoteDaemon) State() (ConnectionState, string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.state, d.stateReason
}

// setState atomically sets the daemon into a new state.
func (d *remoteDaemon) setState(newState ConnectionState) {
	d.setStateWithReason(newState, "")
}

// setStateWithReason sets the daemon into a new state with a reason.
func (d *remoteDaemon) setStateWithReason(newState ConnectionState, reason string) {
	d.mu.Lock()
	d.state = newState
	d.stateReason = reason
	d.mu.Unlock()
}

// HomeDir returns the remote $HOME directory discovered during initialization.
func (d *remoteDaemon) HomeDir() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.homeDir
}

// RemoteClientConn returns the gRPC client connection to the remote daemon.
func (d *remoteDaemon) RemoteClientConn() *grpc.ClientConn {
	cc, err := d.lockedRemoteClientConn()
	if err != nil {
		slog.Error("error getting remote client conn", "error", err)
	}

	return cc
}

// lockedRemoteClientConn returns the gRPC client connection under the mutex.
func (d *remoteDaemon) lockedRemoteClientConn() (*grpc.ClientConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.remoteConn == nil {
		return nil, errors.New("connection is not available")
	}

	return d.remoteConn.ClientConn(), nil
}

// AvailableCommands returns the commands discovered on the remote.
func (d *remoteDaemon) AvailableCommands() []string {
	return *d.availableCommands.Load()
}

// PortForwardStatuses returns the current status of all port forwards.
func (d *remoteDaemon) PortForwardStatuses() []*graftv1.PortForwardStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	statuses := make([]*graftv1.PortForwardStatus, 0, len(d.portForwards))

	for _, fwd := range d.portForwards {
		statuses = append(statuses, &graftv1.PortForwardStatus{
			RemotePort:     fwd.remotePort,
			LocalPort:      fwd.localPort,
			Protocol:       fwd.protocol,
			Conflict:       fwd.conflict,
			ConflictReason: fwd.conflictReason,
			Explicit:       fwd.explicit,
		})
	}

	return statuses
}

type portForwardOptions struct {
	localPort uint32
	explicit  bool
}

type portForwardOption func(*portForwardOptions)

func withLocalPort(localPort uint32) portForwardOption {
	return func(o *portForwardOptions) {
		o.localPort = localPort
	}
}

func withExplicit() portForwardOption {
	return func(o *portForwardOptions) {
		o.explicit = true
	}
}

// AddExplicitPortForward adds a user-requested port forward. Explicit port forwards
// survive auto-detection reconciliation; they are never torn down by the auto-detection
// lifecycle. Returns an error if the local port is already in use.
func (d *remoteDaemon) AddExplicitPortForward(ctx context.Context, spec PortForwardSpec) error {
	localPort := spec.EffectiveLocalPort()
	protocol := spec.EffectiveProtocol()
	key := portForwardKey(protocol, spec.RemotePort)

	d.mu.Lock()

	// Check if this explicit forward already exists.
	if _, ok := d.explicitPorts[key]; ok {
		d.mu.Unlock()

		return nil
	}

	// Record the explicit intent.
	d.explicitPorts[key] = spec

	// If an auto-detected forward already exists for this remote port, replace it
	// (it may have a different local port or we need to mark it explicit).
	if existing, ok := d.portForwards[key]; ok {
		if existing.localPort == localPort && !existing.conflict {
			// Same local port, just mark as explicit.
			existing.explicit = true

			d.mu.Unlock()

			return nil
		}

		// Different local port or conflicted; tear down and restart.
		if existing.listener != nil {
			existing.listener.Close()
		}

		existing.cancel()
		existing.relayCancel()
		delete(d.portForwards, key)
	}

	d.mu.Unlock()

	portInfo := &graftv1.PortInfo{
		Port:     spec.RemotePort,
		Host:     "127.0.0.1",
		Protocol: protocol,
	}

	// Use the daemon's long-lived context, not the caller's request context.
	// The caller is typically a gRPC handler whose context is canceled when
	// the RPC returns, which would immediately tear down the listener and
	// relay goroutines.
	fwdCtx := d.runCtx
	if fwdCtx == nil {
		fwdCtx = ctx
	}

	newFwd := d.startPortForward(fwdCtx, portInfo, withLocalPort(localPort), withExplicit())

	if newFwd.conflict {
		d.mu.Lock()
		delete(d.explicitPorts, key)
		d.mu.Unlock()

		return errors.Errorf("port %d already in use locally: %s", localPort, newFwd.conflictReason)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		newFwd.cancel()
		newFwd.relayCancel()

		return errors.New("daemon is closed")
	}

	d.portForwards[key] = newFwd

	return nil
}

// RemoveExplicitPortForward removes a user-requested port forward. Returns true if the
// port was an explicit forward and was removed, false if it was only auto-detected.
func (d *remoteDaemon) RemoveExplicitPortForward(spec PortForwardSpec) bool {
	key := portForwardKey(spec.EffectiveProtocol(), spec.RemotePort)

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.explicitPorts[key]; !ok {
		return false
	}

	delete(d.explicitPorts, key)

	// Cancel the active forward. On the next reconciliation, if auto-detection
	// sees this port, it will re-create it as auto-detected.
	if fwd, ok := d.portForwards[key]; ok {
		if fwd.listener != nil {
			fwd.listener.Close()
		}

		fwd.cancel()
		fwd.relayCancel()
		delete(d.portForwards, key)
	}

	return true
}

// IsExplicitPortForward returns true if the given port spec is currently an explicit forward.
func (d *remoteDaemon) IsExplicitPortForward(protocol string, remotePort uint32) bool {
	spec := PortForwardSpec{Protocol: protocol, RemotePort: remotePort}
	key := portForwardKey(spec.EffectiveProtocol(), remotePort)

	d.mu.Lock()
	defer d.mu.Unlock()

	_, ok := d.explicitPorts[key]

	return ok
}

// Destination returns the connector's destination URI.
func (d *remoteDaemon) Destination() string {
	return d.connector.Destination()
}

// SafeDestination returns the connector's display-safe destination.
func (d *remoteDaemon) SafeDestination() string {
	return d.connector.SafeDestination()
}

// StateFields returns connector state fields for logging.
func (d *remoteDaemon) StateFields() []any {
	return d.connector.StateFields()
}

// errDaemonSuperseded is a sentinel returned by an afterTransport callback to signal
// that an existing daemon already handles this destination and this daemon should
// stop initializing.
var errDaemonSuperseded = errors.NewBare("daemon superseded by existing")

// Initialize sets up the transport and installs the remote daemon. It uses a
// channel-based blocking pattern: the first caller does the work while
// concurrent callers block on an internal channel until initialization completes.
// The optional afterTransport callback runs after the transport is established
// (hostnames resolved) but before the remote daemon is installed; the
// ConnectionManager uses it to re-key daemons after SSH config resolution.
func (d *remoteDaemon) Initialize(ctx context.Context, afterTransport func() error) error {
	d.mu.Lock()

	// If initialization was already started or completed, wait for the result.
	// This handles concurrent callers and callers arriving after init finishes.
	if d.initDone != nil {
		ch := d.initDone
		d.mu.Unlock()

		select {
		case <-ch:
			return d.initErr
		case <-ctx.Done():
			return errors.Wrap(context.Cause(ctx))
		}
	}

	switch d.state {
	case ConnectionStateNew:
		d.state = ConnectionStateInitializing
		d.initDone = make(chan struct{})
		d.mu.Unlock()

		d.initErr = d.doInitialize(ctx, afterTransport)
		close(d.initDone)

		return d.initErr

	case ConnectionStateConnected:
		d.mu.Unlock()

		return nil

	case ConnectionStateInitializing,
		ConnectionStateFailed,
		ConnectionStateClosed,
		ConnectionStateReconnecting:
		state := d.state
		d.mu.Unlock()

		return errors.Errorf("cannot Initialize daemon in state %s", state)
	}

	panic("unreachable")
}

// doInitialize performs the actual initialization: transport setup, optional
// callback, and daemon installation.
func (d *remoteDaemon) doInitialize(ctx context.Context, afterTransport func() error) error {
	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()

	_, err := d.connector.InitializeRemote(initCtx)
	if err != nil {
		d.setState(ConnectionStateFailed)

		if deinitErr := d.connector.DeinitializeRemote(ctx); deinitErr != nil {
			slog.ErrorContext(ctx, "error deinitializing remote", "error", deinitErr)
		}

		return errors.Wrap(err)
	}

	if afterTransport != nil {
		if err := afterTransport(); err != nil {
			if !errors.Is(err, errDaemonSuperseded) {
				d.setState(ConnectionStateFailed)

				if deinitErr := d.connector.DeinitializeRemote(ctx); deinitErr != nil {
					slog.ErrorContext(ctx, "error deinitializing remote", "error", deinitErr)
				}
			}

			return err
		}
	}

	if err := d.ensureDaemon(ctx); err != nil {
		d.setState(ConnectionStateFailed)

		if deinitErr := d.connector.DeinitializeRemote(ctx); deinitErr != nil {
			slog.ErrorContext(ctx, "error deinitializing remote", "error", deinitErr)
		}

		return err
	}

	d.setState(ConnectionStateConnected)

	d.monitor()
	d.monitorPorts()

	return nil
}

// setConnectedState sets the daemon into the Connected state with discovered info.
func (d *remoteDaemon) setConnectedState(
	homeDir string,
	remoteSocketPath string,
	remoteConn RemoteDaemonConnection,
) {
	d.mu.Lock()
	d.homeDir = homeDir
	d.remoteSocketPath = remoteSocketPath
	d.remoteConn = remoteConn
	d.mu.Unlock()

	d.availableCommands.Store(&[]string{})
}

// discover runs OS/arch/home directory detection on the remote host, or returns
// cached results if another connection has already discovered.
// Safe to call under installMu (lock order: installMu → discoverMu).
func (d *remoteDaemon) discover(ctx context.Context) (discoveryInfo, error) {
	d.discoverMu.Lock()
	defer d.discoverMu.Unlock()

	if d.discoveryDone {
		return d.discovery, d.discoveryErr
	}

	info, err := discoverRemote(ctx, d.connector)

	d.discovery = info
	d.discoveryErr = err
	d.discoveryDone = true

	return info, err
}

// resetDiscovery clears cached discovery results so the next Discover call
// re-runs remote detection.
func (d *remoteDaemon) resetDiscovery() {
	d.discoverMu.Lock()
	defer d.discoverMu.Unlock()

	d.discoveryDone = false
	d.discovery = discoveryInfo{}
	d.discoveryErr = nil
}

// alreadyInstalled reports whether the daemon has already been installed
// in this lifecycle. Must be called under installMu.
func (d *remoteDaemon) alreadyInstalled() bool {
	return d.reinstalledOnce
}

// markInstalled records that the daemon has been installed.
// Must be called under installMu.
func (d *remoteDaemon) markInstalled() {
	d.reinstalledOnce = true
}

// resetInstallState clears the installed flag. Must be called under installMu.
func (d *remoteDaemon) resetInstallState() {
	d.reinstalledOnce = false
}

// ensureDaemon (re)installs the graft daemon at the remote destination.
func (d *remoteDaemon) ensureDaemon(ctx context.Context) error {
	d.installMu.Lock()
	defer d.installMu.Unlock()

	info, err := d.discover(ctx)
	if err != nil {
		return err
	}

	graftBinPath, err := daemonBinPath(ctx, info.OS, info.Arch)
	if err != nil {
		return errors.WrapPrefix(err, "error getting daemon binary path")
	}

	daemonPath, err := d.remoteDaemonBinPath(ctx, info.HomeDir, graftBinPath, info.RemoteSocketPath)
	if err != nil {
		return err
	}

	remoteConn, daemonIsOnline := d.tryConnectExistingDaemon(ctx, daemonPath, info.RemoteSocketPath)

	var success bool

	defer func() {
		if !success && remoteConn != nil {
			if disErr := remoteConn.Close(); disErr != nil {
				slog.DebugContext(ctx, "error disconnecting from remote daemon", "error", disErr)
			}
		}
	}()

	reinstall := !daemonIsOnline || remoteConn == nil
	if reinstall {
		d.resetInstallState()
	}

	if !reinstall {
		var checkErr error

		reinstall, checkErr = d.checkReinstallNeeded(ctx, remoteConn)
		if checkErr != nil {
			return checkErr
		}
	}

	if reinstall {
		slog.DebugContext(ctx, "need to reinstall daemon")

		if reinstallErr := d.reinstallDaemon(
			ctx,
			info.HomeDir,
			daemonPath,
			graftBinPath,
			remoteConn,
			true,
		); reinstallErr != nil {
			return reinstallErr
		}

		if remoteConn != nil {
			if disErr := remoteConn.Close(); disErr != nil {
				slog.DebugContext(ctx, "error disconnecting from remote daemon", "error", disErr)
			}
		}

		var ok bool

		remoteConn, ok, err = d.connector.ConnectToRemoteDaemon(ctx, daemonPath, info.RemoteSocketPath)
		if err != nil {
			slog.ErrorContext(ctx, "error connecting to remote daemon", "error", err)

			return errors.WrapPrefix(err, "error connecting to remote daemon")
		}

		if !ok {
			return errors.New("invariant: expected to have connection after connecting to remote daemon")
		}
	}

	d.setConnectedState(info.HomeDir, info.RemoteSocketPath, remoteConn)

	success = true

	return nil
}

// remoteDaemonBinPath returns the path where the daemon binary should be placed on the remote.
func (d *remoteDaemon) remoteDaemonBinPath(
	ctx context.Context, homeDir, graftBinPath, remoteSocketPath string,
) (string, error) {
	binBase := filepath.Base(graftBinPath)

	identity := d.connector.Identity()
	if identity == "" {
		return filepath.Join(homeDir, binBase), nil
	}

	daemonDir := filepath.Dir(remoteSocketPath)
	if _, mkErr := d.connector.RunOneShotCommand(ctx, "mkdir -p "+daemonDir); mkErr != nil {
		return "", errors.WrapPrefix(mkErr, "error creating daemon directory")
	}

	return filepath.Join(daemonDir, binBase), nil
}

// tryConnectExistingDaemon checks if a daemon binary exists and tries to connect.
func (d *remoteDaemon) tryConnectExistingDaemon(
	ctx context.Context, daemonPath, remoteSocketPath string,
) (RemoteDaemonConnection, bool) {
	_, statErr := d.connector.RunOneShotCommand(ctx, "stat "+daemonPath)
	if statErr != nil {
		slog.DebugContext(ctx, "error running stat", "error", statErr)

		return nil, false
	}

	slog.DebugContext(ctx, "checking if daemon is already running")

	remoteConn, daemonIsOnline, connectErr := d.connector.ConnectToRemoteDaemon(ctx, daemonPath, remoteSocketPath)
	if connectErr != nil {
		slog.ErrorContext(ctx, "error connecting to remote daemon", "error", connectErr)
	}

	return remoteConn, daemonIsOnline
}

// checkReinstallNeeded returns whether the daemon should be reinstalled.
func (d *remoteDaemon) checkReinstallNeeded(ctx context.Context, candRemoteConn RemoteDaemonConnection) (bool, error) {
	const tempDialTimeout = 3 * time.Second

	tempDialCtx, cancelTempDial := context.WithTimeout(ctx, tempDialTimeout)
	defer cancelTempDial()

	remClient := graftv1.NewGraftServiceClient(candRemoteConn.ClientConn())

	resp, statusErr := remClient.Status(tempDialCtx, &graftv1.StatusRequest{})
	if statusErr != nil {
		slog.ErrorContext(ctx, "error getting system status", "error", statusErr)

		return true, nil
	}

	if reason := BuildVersionsEqual(resp.GetVersionInfo(), ourVersion); reason != "" {
		slog.InfoContext(
			ctx,
			"local daemon version differs from remote",
			"why", reason,
			"destination", d.connector.Destination(),
			"local_version", versionString(ourVersion),
			"remote_version", versionString(resp.GetVersionInfo()))

		return true, nil
	}

	slog.DebugContext(ctx, "no need to reinstall daemon",
		"destination", d.connector.Destination(),
		"local_version", versionString(ourVersion),
		"remote_version", versionString(resp.GetVersionInfo()),
	)

	return false, nil
}

// reinstallDaemon copies graft and starts it up as a daemon on the remote.
func (d *remoteDaemon) reinstallDaemon(
	ctx context.Context,
	homeDir string,
	daemonPath string,
	graftBinPath string,
	remoteConn RemoteDaemonConnection,
	replace bool,
) error {
	if d.alreadyInstalled() {
		return nil
	}

	d.markInstalled()

	fmt.Fprintln(OOBWriterFromContext(ctx), "installing daemon")
	slog.DebugContext(ctx, "installing daemon on remote", "destination", d.connector.Destination())

	if remoteConn != nil {
		slog.DebugContext(ctx, "shutting down existing daemon")

		remClient := graftv1.NewGraftServiceClient(remoteConn.ClientConn())
		if _, shutdownErr := remClient.Shutdown(ctx, &graftv1.ShutdownRequest{}); shutdownErr != nil {
			slog.ErrorContext(ctx, "error shutting down daemon; will try to continue", "error", shutdownErr)
		}

		slog.DebugContext(ctx, "waiting a bit post remote shutdown")

		select {
		case <-ctx.Done():
			return errors.Wrap(context.Cause(ctx))
		case <-time.After(3 * time.Second):
		}
	}

	slog.DebugContext(ctx, "copying graft to remote", "to", homeDir)

	// Remove the old binary first to avoid ETXTBSY ("Text file busy") on Linux.
	// If the previous daemon process is still running, unlinking the directory
	// entry lets it keep its inode while we write a fresh file at the same path.
	rmCmd := "rm -f " + daemonPath
	if _, rmErr := d.connector.RunOneShotCommand(ctx, rmCmd); rmErr != nil {
		slog.DebugContext(ctx, "error removing old daemon binary; will try to continue", "error", rmErr)
	}

	if copyErr := d.connector.CopyFile(
		ctx,
		graftBinPath,
		daemonPath,
		"770",
	); copyErr != nil {
		slog.ErrorContext(ctx, "error copying daemon", "error", copyErr)

		return errors.Wrap(copyErr)
	}

	daemonArgs := daemonPath + " daemon --as-remote"
	if identity := d.connector.Identity(); identity != "" {
		daemonArgs += " --identity " + identity
	}

	if replace {
		daemonArgs += " --replace"
	}

	cmd := fmt.Sprintf("GRAFT_LOG_LEVEL=%s bash -ic '%s'", d.logLevel.String(), daemonArgs)
	slog.DebugContext(ctx, "starting daemon on remote", "cmd", cmd)

	if _, runErr := d.connector.RunOneShotCommand(ctx, cmd); runErr != nil {
		slog.ErrorContext(ctx, "error starting daemon", "error", runErr)

		return errors.WrapPrefix(runErr, "error starting daemon")
	}

	slog.DebugContext(ctx, "done (re)installing", "cmd", cmd)

	return nil
}

const (
	reconnectInitialBackoff = time.Second
	reconnectMaxBackoff     = 30 * time.Second
)

// teardownForReconnect cancels monitors and port forwards, closes the old
// remote connection and connector transport.
func (d *remoteDaemon) teardownForReconnect(ctx context.Context) {
	d.mu.Lock()

	if d.cancelMonitor != nil {
		d.cancelMonitor()
		d.cancelMonitor = nil
	}

	if d.cancelMonitorPort != nil {
		d.cancelMonitorPort()
		d.cancelMonitorPort = nil
	}

	for key, fwd := range d.portForwards {
		fwd.cancel()
		fwd.relayCancel()
		delete(d.portForwards, key)
	}

	remoteConn := d.remoteConn
	d.remoteConn = nil
	d.mu.Unlock()

	if remoteConn != nil {
		if err := remoteConn.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing old remote connection during reconnect", "error", err)
		}
	}

	if err := d.connector.Close(); err != nil {
		slog.ErrorContext(ctx, "error closing connector during reconnect", "error", err)
	}
}

// reconnectOnce tries a single reconnect attempt.
func (d *remoteDaemon) reconnectOnce(ctx context.Context) bool {
	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()

	if _, err := d.connector.InitializeRemote(initCtx); err != nil {
		slog.ErrorContext(ctx, "error re-initializing transport during reconnect", "error", err)

		return false
	}

	d.resetDiscovery()

	if err := d.ensureDaemon(ctx); err != nil {
		slog.ErrorContext(ctx, "error ensuring daemon during reconnect", "error", err)

		return false
	}

	return true
}

// Reconnect attempts to re-establish a broken connection, retrying with
// exponential backoff until it succeeds or the context is canceled.
func (d *remoteDaemon) Reconnect(runCtx context.Context) bool {
	d.mu.Lock()

	if d.closed || d.reconnecting {
		d.mu.Unlock()

		return false
	}

	d.reconnecting = true
	d.state = ConnectionStateReconnecting
	d.mu.Unlock()

	d.teardownForReconnect(runCtx)

	backoff := reconnectInitialBackoff

	for {
		if d.reconnectOnce(runCtx) {
			d.mu.Lock()

			if d.closed {
				d.reconnecting = false
				d.mu.Unlock()

				return false
			}

			d.state = ConnectionStateConnected
			d.reconnecting = false
			d.mu.Unlock()

			slog.InfoContext(runCtx, "reconnected successfully", "destination", d.connector.Destination())

			d.monitor()
			d.monitorPorts()

			return true
		}

		d.mu.Lock()

		if d.closed {
			d.reconnecting = false
			d.mu.Unlock()

			return false
		}

		d.mu.Unlock()

		slog.WarnContext(runCtx, "reconnect attempt failed, retrying",
			"destination", d.connector.Destination(), "backoff", backoff)

		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, reconnectMaxBackoff) //nolint:mnd // exponential backoff doubling
		case <-runCtx.Done():
			d.mu.Lock()
			d.reconnecting = false
			d.mu.Unlock()

			return false
		}
	}
}

// monitor starts command discovery on the remote daemon.
// No-op if already monitoring.
func (d *remoteDaemon) monitor() {
	d.mu.Lock()

	if d.cancelMonitor != nil {
		d.mu.Unlock()

		return
	}

	d.activeWorkers.Add(1)

	cancelCtx, cancelMonitor := context.WithCancel(d.runCtx)
	d.cancelMonitor = cancelMonitor
	d.mu.Unlock()

	go func() {
		defer d.activeWorkers.Done()

		cc, err := d.lockedRemoteClientConn()
		if err != nil {
			return
		}

		remClient := graftv1.NewGraftServiceClient(cc)

		var directories []string
		if roots := d.remoteRoots.Load(); roots != nil {
			directories = *roots
		}

		commandsClient, err := remClient.DiscoverCommands(cancelCtx, &graftv1.DiscoverCommandsRequest{
			Directories: directories,
		})
		if err != nil {
			slog.ErrorContext(cancelCtx, "error starting DiscoverCommands", "error", err)

			return
		}

		for {
			resp, err := commandsClient.Recv()
			if err != nil {
				if !IsCanceledError(err) {
					slog.ErrorContext(cancelCtx, "error receiving DiscoverCommands", "error", err)
				}

				return
			}

			d.availableCommands.Store(&resp.Commands)
		}
	}()
}

// monitorPorts starts port watching and forwarding on the remote daemon.
// No-op if already monitoring.
func (d *remoteDaemon) monitorPorts() {
	d.mu.Lock()

	if d.cancelMonitorPort != nil {
		d.mu.Unlock()

		return
	}

	d.activeWorkers.Add(1)

	cancelCtx, cancelMonitorPort := context.WithCancel(d.runCtx)
	d.cancelMonitorPort = cancelMonitorPort
	d.mu.Unlock()

	go func() {
		defer d.activeWorkers.Done()

		d.watchAndForwardPorts(cancelCtx)
	}()
}

func (d *remoteDaemon) watchAndForwardPorts(ctx context.Context) {
	cc, err := d.lockedRemoteClientConn()
	if err != nil {
		return
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	backoff := time.Second

	const maxBackoff = 30 * time.Second

	for {
		start := time.Now()

		err := d.runPortWatchStream(ctx, remClient)

		if IsCanceledError(err) {
			return
		}

		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()

		if closed {
			return
		}

		if time.Since(start) > maxBackoff {
			backoff = time.Second
		}

		slog.WarnContext(ctx, "port watch stream failed, reconnecting", "error", err, "backoff", backoff)

		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff) //nolint:mnd // exponential backoff doubling
		case <-ctx.Done():
			return
		}
	}
}

func (d *remoteDaemon) runPortWatchStream(
	ctx context.Context,
	remClient graftv1.GraftServiceClient,
) error {
	watchClient, err := remClient.WatchPorts(ctx, &graftv1.WatchPortsRequest{})
	if err != nil {
		return errors.Wrap(err)
	}

	type recvResult struct {
		resp *graftv1.WatchPortsResponse
		err  error
	}

	recvCh := make(chan recvResult, 1)

	go func() {
		for {
			resp, recvErr := watchClient.Recv()
			recvCh <- recvResult{resp, recvErr}

			if recvErr != nil {
				return
			}
		}
	}()

	const conflictRetryInterval = 3 * time.Second

	conflictRetry := time.NewTicker(conflictRetryInterval)
	defer conflictRetry.Stop()

	var lastPorts []*graftv1.PortInfo

	for {
		select {
		case result := <-recvCh:
			if result.err != nil {
				return errors.Wrap(result.err)
			}

			lastPorts = result.resp.GetPorts()
			d.reconcilePortForwards(ctx, lastPorts)
		case <-conflictRetry.C:
			if lastPorts != nil && d.hasConflictedForwards() {
				d.reconcilePortForwards(ctx, lastPorts)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (d *remoteDaemon) hasConflictedForwards() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, fwd := range d.portForwards {
		if fwd.conflict {
			return true
		}
	}

	return false
}

// reconcilePortForwards is only called serially from the single runPortWatchStream goroutine.
//
//nolint:gocognit // reconciliation logic with explicit port injection is inherently branchy
func (d *remoteDaemon) reconcilePortForwards(ctx context.Context, remotePorts []*graftv1.PortInfo) {
	d.mu.Lock()

	remoteSet := make(map[string]*graftv1.PortInfo, len(remotePorts))
	for _, p := range remotePorts {
		remoteSet[portForwardKey(p.GetProtocol(), p.GetPort())] = p
	}

	// Inject explicit ports so they are never torn down by auto-detection disappearing.
	for key, spec := range d.explicitPorts {
		if _, ok := remoteSet[key]; !ok {
			remoteSet[key] = &graftv1.PortInfo{
				Port:     spec.RemotePort,
				Host:     "127.0.0.1",
				Protocol: spec.EffectiveProtocol(),
			}
		}
	}

	for key, fwd := range d.portForwards {
		if _, ok := remoteSet[key]; !ok {
			slog.InfoContext(ctx, "stopping port forward", "protocol", fwd.protocol, "port", fwd.remotePort)

			if fwd.listener != nil {
				fwd.listener.Close()
			}

			fwd.cancel()
			delete(d.portForwards, key)
		}
	}

	type portToStart struct {
		info *graftv1.PortInfo
		opts []portForwardOption
	}

	// explicitOptsForKey returns port forward options if the key corresponds to an explicit forward.
	// Must be called under d.mu.
	explicitOptsForKey := func(key string) []portForwardOption {
		spec, isExplicit := d.explicitPorts[key]
		if !isExplicit {
			return nil
		}

		return []portForwardOption{withLocalPort(spec.EffectiveLocalPort()), withExplicit()}
	}

	toStart := map[string]portToStart{}

	for key, fwd := range d.portForwards {
		if !fwd.conflict {
			continue
		}

		if p, ok := remoteSet[key]; ok {
			if fwd.listener != nil {
				fwd.listener.Close()
			}

			fwd.cancel()
			delete(d.portForwards, key)

			toStart[key] = portToStart{info: p, opts: explicitOptsForKey(key)}
		}
	}

	for key, p := range remoteSet {
		if _, ok := d.portForwards[key]; ok {
			continue
		}

		if _, retrying := toStart[key]; !retrying {
			toStart[key] = portToStart{info: p, opts: explicitOptsForKey(key)}
		}
	}

	d.mu.Unlock()

	newForwards := make(map[string]*activePortForward, len(toStart))

	for key, pts := range toStart {
		newFwd := d.startPortForward(ctx, pts.info, pts.opts...)
		newForwards[key] = newFwd

		if !newFwd.conflict {
			slog.InfoContext(ctx, "forwarding port", "protocol", newFwd.protocol, "port", newFwd.remotePort,
				"localPort", newFwd.localPort, "explicit", newFwd.explicit)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		for _, fwd := range newForwards {
			fwd.cancel()
			fwd.relayCancel()
		}

		return
	}

	maps.Copy(d.portForwards, newForwards)
}

func (d *remoteDaemon) startPortForward(ctx context.Context, portInfo *graftv1.PortInfo, opts ...portForwardOption) *activePortForward {
	port := portInfo.GetPort()
	protocol := portInfo.GetProtocol()
	host := portInfo.GetHost()

	var pfo portForwardOptions
	for _, o := range opts {
		o(&pfo)
	}

	localPort := port
	if pfo.localPort != 0 {
		localPort = pfo.localPort
	}

	relayCtx, relayCancel := context.WithCancel(ctx)
	fwdCtx, cancel := context.WithCancel(relayCtx)

	fwd := &activePortForward{
		remotePort:  port,
		localPort:   localPort,
		protocol:    protocol,
		cancel:      cancel,
		relayCancel: relayCancel,
		explicit:    pfo.explicit,
	}

	if conflict, reason := probePortConflict(protocol, localPort); conflict {
		fwd.conflict = true
		fwd.conflictReason = reason

		cancel()
		relayCancel()

		return fwd
	}

	if protocol == "udp" {
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", localPort))
		if err != nil {
			fwd.conflict = true
			fwd.conflictReason = err.Error()

			cancel()
			relayCancel()

			return fwd
		}

		udpConn, err := net.ListenUDP("udp", addr)
		if err != nil {
			fwd.conflict = true
			fwd.conflictReason = fmt.Sprintf("port %d already in use locally", localPort)

			cancel()
			relayCancel()

			return fwd
		}

		d.activeWorkers.Go(func() {
			defer udpConn.Close()

			d.handleUDPForward(relayCtx, fwdCtx, udpConn, host, port)
		})

		return fwd
	}

	// TCP
	var lc net.ListenConfig

	listener, err := lc.Listen(fwdCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		fwd.conflict = true
		fwd.conflictReason = fmt.Sprintf("port %d already in use locally", localPort)

		cancel()
		relayCancel()

		return fwd
	}

	fwd.listener = listener

	d.activeWorkers.Go(func() {
		defer listener.Close()

		d.handleTCPForward(relayCtx, fwdCtx, listener, host, port)
	})

	return fwd
}

func (d *remoteDaemon) handleTCPForward(
	relayCtx context.Context,
	listenerCtx context.Context,
	listener net.Listener,
	host string,
	port uint32,
) {
	d.activeWorkers.Go(func() {
		<-listenerCtx.Done()
		listener.Close()
	})

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}

		d.activeWorkers.Go(func() {
			defer localConn.Close()

			d.relayTCPConnection(relayCtx, localConn, host, port)
		})
	}
}

//nolint:gocognit,cyclop // bidirectional relay inherently has branching
func (d *remoteDaemon) relayTCPConnection(ctx context.Context, localConn net.Conn, host string, port uint32) {
	cc, err := d.lockedRemoteClientConn()
	if err != nil {
		return
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	stream, err := remClient.ForwardPort(streamCtx)
	if err != nil {
		slog.ErrorContext(ctx, "error opening ForwardPort stream", "error", err)

		return
	}

	if err := stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Start{
			Start: &graftv1.ForwardPortStart{
				Port:     port,
				Host:     host,
				Protocol: "tcp",
			},
		},
	}); err != nil {
		slog.ErrorContext(ctx, "error sending ForwardPortStart", "error", err)

		return
	}

	errCh := make(chan error, 2) //nolint:mnd // two directions

	// local -> gRPC
	go func() {
		buf := make([]byte, 32*1024)

		for {
			n, readErr := localConn.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&graftv1.ForwardPortRequest{
					Data: &graftv1.ForwardPortRequest_Payload{
						Payload: buf[:n],
					},
				}); sendErr != nil {
					errCh <- sendErr

					return
				}
			}

			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					if closeErr := stream.CloseSend(); closeErr != nil {
						errCh <- closeErr

						return
					}

					errCh <- nil
				} else {
					errCh <- readErr
				}

				return
			}
		}
	}()

	// gRPC -> local
	go func() {
		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					if tc, ok := localConn.(*net.TCPConn); ok {
						tc.CloseWrite() //nolint:errcheck // best-effort half-close
					}

					errCh <- nil
				} else {
					errCh <- recvErr
				}

				return
			}

			if len(resp.GetPayload()) > 0 {
				if _, writeErr := localConn.Write(resp.GetPayload()); writeErr != nil {
					errCh <- writeErr

					return
				}
			}
		}
	}()

	err1 := <-errCh
	if err1 != nil {
		localConn.Close()
		streamCancel()

		<-errCh

		slog.ErrorContext(ctx, "relay error", "error", err1)

		return
	}

	err2 := <-errCh
	if err2 != nil {
		slog.ErrorContext(ctx, "relay error", "error", err2)
	}
}

// udpSession tracks a per-client-address UDP relay stream.
type udpSession struct {
	stream   graftv1.GraftService_ForwardPortClient
	lastUsed time.Time
	cancel   context.CancelFunc
}

const udpSessionTimeout = 30 * time.Second

//nolint:gocognit,cyclop // UDP session management inherently has branching
func (d *remoteDaemon) handleUDPForward(
	relayCtx context.Context,
	listenerCtx context.Context,
	udpConn *net.UDPConn,
	host string,
	port uint32,
) {
	d.activeWorkers.Go(func() {
		<-listenerCtx.Done()
		udpConn.Close()
	})

	sessions := map[string]*udpSession{}

	var sessionsMu sync.Mutex

	d.activeWorkers.Go(func() {
		ticker := time.NewTicker(udpSessionTimeout / 2) //nolint:mnd // reap at half the timeout
		defer ticker.Stop()

		for {
			select {
			case <-listenerCtx.Done():
				sessionsMu.Lock()

				for _, s := range sessions {
					s.cancel()
				}

				sessions = map[string]*udpSession{}

				sessionsMu.Unlock()

				return
			case <-ticker.C:
				sessionsMu.Lock()

				now := time.Now()
				for key, s := range sessions {
					if now.Sub(s.lastUsed) > udpSessionTimeout {
						s.cancel()
						delete(sessions, key)
					}
				}

				sessionsMu.Unlock()
			}
		}
	})

	buf := make([]byte, 65535)

	for {
		n, clientAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		payload := make([]byte, n)
		copy(payload, buf[:n])

		sessionKey := clientAddr.String()

		sessionsMu.Lock()

		sess, ok := sessions[sessionKey]

		if ok {
			sess.lastUsed = time.Now()
		}

		sessionsMu.Unlock()

		if ok {
			if sendErr := sess.stream.Send(&graftv1.ForwardPortRequest{
				Data: &graftv1.ForwardPortRequest_Payload{
					Payload: payload,
				},
			}); sendErr != nil {
				sess.cancel()
				sessionsMu.Lock()
				delete(sessions, sessionKey)
				sessionsMu.Unlock()

				ok = false
			}
		}

		if !ok {
			sess = d.startUDPSession(relayCtx, udpConn, clientAddr, host, port, payload)
			if sess == nil {
				continue
			}

			sessionsMu.Lock()

			sessions[sessionKey] = sess

			sessionsMu.Unlock()

			d.activeWorkers.Go(func() {
				defer func() {
					sess.cancel()
					sessionsMu.Lock()
					delete(sessions, sessionKey)
					sessionsMu.Unlock()
				}()

				for {
					resp, recvErr := sess.stream.Recv()
					if recvErr != nil {
						return
					}

					if len(resp.GetPayload()) > 0 {
						if _, writeErr := udpConn.WriteToUDP(resp.GetPayload(), clientAddr); writeErr != nil {
							return
						}
					}
				}
			})
		}
	}
}

func (d *remoteDaemon) startUDPSession(
	ctx context.Context,
	_ *net.UDPConn,
	_ *net.UDPAddr,
	host string,
	port uint32,
	initialPayload []byte,
) *udpSession {
	cc, err := d.lockedRemoteClientConn()
	if err != nil {
		return nil
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	sessCtx, sessCancel := context.WithCancel(ctx)

	stream, err := remClient.ForwardPort(sessCtx)
	if err != nil {
		if !IsCanceledError(err) {
			slog.ErrorContext(ctx, "error opening ForwardPort stream for UDP", "error", err)
		}

		sessCancel()

		return nil
	}

	if err := stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Start{
			Start: &graftv1.ForwardPortStart{
				Port:     port,
				Host:     host,
				Protocol: "udp",
			},
		},
	}); err != nil {
		sessCancel()

		return nil
	}

	if err := stream.Send(&graftv1.ForwardPortRequest{
		Data: &graftv1.ForwardPortRequest_Payload{
			Payload: initialPayload,
		},
	}); err != nil {
		sessCancel()

		return nil
	}

	return &udpSession{
		stream:   stream,
		lastUsed: time.Now(),
		cancel:   sessCancel,
	}
}

// DumpLogs dumps all found daemon logs in a single round-trip.
func (d *remoteDaemon) DumpLogs(ctx context.Context) (string, string, error) {
	logsPath, err := DaemonLogsPathForRemote(d.HomeDir())
	if err != nil {
		return "", "", err
	}

	outLogPath := filepath.Join(logsPath, "out.log")
	errLogPath := filepath.Join(logsPath, "error.log")

	const separator = "---GRAFT_LOG_SEPARATOR---"

	combined, err := d.connector.RunOneShotCommand(ctx,
		"cat "+outLogPath+" 2>/dev/null; echo '"+separator+"'; cat "+errLogPath+" 2>/dev/null")
	if err != nil {
		return "", "", errors.WrapPrefix(err, "error getting daemon logs")
	}

	parts := strings.SplitN(combined, separator+"\n", 2)
	outLogs := parts[0]

	var errLogs string
	if len(parts) > 1 {
		errLogs = parts[1]
	}

	return outLogs, errLogs, nil
}

// Close tears down the daemon. Shutdown ordering:
//  1. Cancel monitor and all port forward listener contexts.
//  2. Cancel all relay contexts.
//  3. Close the remote gRPC connection.
//  4. Close the connector (SSH/Docker transport).
//  5. Wait for all active worker goroutines to finish.
func (d *remoteDaemon) Close() error {
	d.mu.Lock()

	if d.closed {
		d.mu.Unlock()

		return nil
	}

	d.closed = true
	d.state = ConnectionStateClosed

	remoteConn := d.remoteConn
	if d.cancelMonitor != nil {
		d.cancelMonitor()
	}

	if d.cancelMonitorPort != nil {
		d.cancelMonitorPort()
	}

	for key, fwd := range d.portForwards {
		fwd.cancel()
		fwd.relayCancel()
		delete(d.portForwards, key)
	}

	d.mu.Unlock()

	defer d.activeWorkers.Wait()

	var remoteCloseErr error
	if remoteConn != nil {
		remoteCloseErr = remoteConn.Close()
	}

	return errors.Join(remoteCloseErr, d.connector.Close())
}

// Destroy closes the daemon and deinitializes the remote (e.g. removes Docker container).
func (d *remoteDaemon) Destroy(ctx context.Context) error {
	closeErr := d.Close()

	return errors.Join(d.connector.DeinitializeRemote(ctx), closeErr)
}

var errDetectingOSArch = errors.NewBare("unable to detect destination operating system and architecture")

// parseDetectedOS normalizes a `uname -s` output into a known OS string.
func parseDetectedOS(raw string) (string, error) {
	switch foundOS := strings.ToLower(strings.TrimSpace(raw)); foundOS {
	case osLinux:
		return osLinux, nil
	case osDarwin:
		return osDarwin, nil
	default:
		return "", errors.WrapSuffix(errDetectingOSArch, "found "+foundOS)
	}
}

// parseDetectedArch normalizes a `uname -m` output into a known architecture string.
func parseDetectedArch(raw string) (string, error) {
	switch foundArch := strings.ToLower(strings.TrimSpace(raw)); foundArch {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", errors.WrapSuffix(errDetectingOSArch, "found "+foundArch)
	}
}

// discoverRemote runs remote detection commands to determine OS, arch, and home directory
// in a single round-trip.
func discoverRemote(ctx context.Context, connector RemoteConnector) (discoveryInfo, error) {
	// Combine three commands into one to avoid 3 sequential round-trips.
	output, err := connector.RunOneShotCommand(ctx, unameOSCmd+" && "+unameArchCmd+" && "+homeDirCmd)
	if err != nil {
		return discoveryInfo{}, errors.WrapPrefix(err, "error detecting remote environment")
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 { //nolint:mnd // expecting OS, arch, home
		return discoveryInfo{}, errors.Errorf(
			"expected 3 lines from remote detection, got %d: %q", len(lines), output)
	}

	detOS, err := parseDetectedOS(lines[0])
	if err != nil {
		return discoveryInfo{}, err
	}

	detArch, err := parseDetectedArch(lines[1])
	if err != nil {
		return discoveryInfo{}, err
	}

	homeDir := strings.TrimSpace(lines[2])

	remoteSocketPath, err := DaemonSocketPathForRemote(homeDir, connector.Identity())
	if err != nil {
		return discoveryInfo{}, err
	}

	return discoveryInfo{
		OS:               detOS,
		Arch:             detArch,
		HomeDir:          homeDir,
		RemoteSocketPath: remoteSocketPath,
	}, nil
}

func portForwardKey(protocol string, port uint32) string {
	return protocol + ":" + strconv.FormatUint(uint64(port), 10)
}

// probePortConflict checks whether a port is already in use locally.
func probePortConflict(protocol string, port uint32) (bool, string) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	reason := fmt.Sprintf("port %d already in use locally", port)

	if protocol == "tcp" {
		dialer := net.Dialer{Timeout: 200 * time.Millisecond} //nolint:mnd // short probe timeout

		probeConn, err := dialer.Dial("tcp", addr)
		if err == nil {
			probeConn.Close()

			return true, reason
		}

		return false, ""
	}

	var lc net.ListenConfig

	probeConn, err := lc.ListenPacket(context.Background(), "udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true, reason
	}

	probeConn.Close()

	return false, ""
}

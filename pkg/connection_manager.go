package graft

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/synchronization"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// The ConnectionManager is responsible for the lifetime of [Connection]s. Once a connection is running,
// the manager doesn't do very much other than fetching connections and forwarding methods to them in
// a guarded fashion (when they are in the right state).
type ConnectionManager struct {
	//nolint:containedctx // self-owned
	runCtx              context.Context
	connections         map[string]*Connection
	daemons             map[string]*remoteDaemon // keyed by "host::identity"
	runCtxCancel        func()
	schemes             map[string]ConnectorFactory
	connectionRootsPath string // global file mapping local roots to connection names
	connMgrMu           sync.Mutex
}

// NewConnectionManager returns a non-started ConnectionManager.
func NewConnectionManager() *ConnectionManager {
	runCtx, runCtxCancel := context.WithCancel(context.Background())

	var rootsPath string
	if sessRoot, err := SessionsRoot(); err == nil {
		rootsPath = filepath.Join(filepath.Dir(sessRoot), connectionRootsFileName)
	}

	return &ConnectionManager{
		connections:         map[string]*Connection{},
		daemons:             map[string]*remoteDaemon{},
		schemes:             map[string]ConnectorFactory{},
		connectionRootsPath: rootsPath,
		runCtx:              runCtx,
		runCtxCancel:        runCtxCancel,
	}
}

// RegisterConnectorFactory associates a scheme key to the [ConnectorFactory] factory.
func (mgr *ConnectionManager) RegisterConnectorFactory(key string, scheme ConnectorFactory) {
	slog.DebugContext(context.Background(), "registering connector factory", "scheme", key)
	mgr.connMgrMu.Lock()
	mgr.schemes[key] = scheme
	mgr.connMgrMu.Unlock()
}

const connectionRootsFileName = "connection_roots"

var errConflictingDestinationSameName = errors.NewBare("a different destination exists under the same name")

// getOrCreateDaemonForConnection returns the daemon for an existing connection (if any),
// or creates a new daemon. Returns (daemon, existingConn, error).
func (mgr *ConnectionManager) getOrCreateDaemonForConnection(
	ctx context.Context,
	name string,
	destURL *url.URL,
	identity string,
	scheme ConnectorFactory,
) (*remoteDaemon, *Connection, error) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	if conn, ok := mgr.connections[name]; ok {
		if conn.daemon.Destination() != destURL.String() {
			return nil, nil, errors.WrapSuffix(
				errConflictingDestinationSameName,
				fmt.Sprintf("other_destination='%s',name='%s'", conn.daemon.Destination(), name))
		}

		slog.DebugContext(ctx, "connection already initialized", "destination", destURL.String())

		return conn.daemon, conn, nil
	}

	daemon, err := mgr.getOrCreateDaemon(ctx, destURL, identity, scheme)
	if err != nil {
		return nil, nil, err
	}

	return daemon, nil, nil
}

var errOverlappingLocalRoot = errors.NewBare("local root overlaps with existing connection")

// createConnection creates a new connection backed by the given daemon and registers it.
// It returns an error if a non-background connection's local root overlaps with an
// existing non-background connection's local root.
func (mgr *ConnectionManager) createConnection(
	name, localRoot, remoteRoot string, daemon *remoteDaemon, background bool,
) (*Connection, error) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	if !background && localRoot != "" {
		if err := mgr.checkOverlappingRoots(localRoot); err != nil {
			return nil, err
		}
	}

	conn := newConnection(daemon, name, localRoot, remoteRoot, background)
	mgr.connections[name] = conn
	mgr.updateDaemonRemoteRoots(daemon)
	mgr.writeConnectionRootsFile()

	return conn, nil
}

// checkOverlappingRoots checks whether localRoot overlaps with any existing non-background
// connection's local root. Must be called with connMgrMu held.
func (mgr *ConnectionManager) checkOverlappingRoots(localRoot string) error {
	newRoot := strings.ToLower(localRoot)

	for _, conn := range mgr.connections {
		if conn.background || conn.localRoot == "" {
			continue
		}

		existingRoot := strings.ToLower(conn.localRoot)

		if _, ok := hasPathPrefix(newRoot, existingRoot); ok {
			return errors.WrapSuffix(errOverlappingLocalRoot,
				fmt.Sprintf("'%s' overlaps with connection '%s' (%s)", localRoot, conn.name, conn.localRoot))
		}

		if _, ok := hasPathPrefix(existingRoot, newRoot); ok {
			return errors.WrapSuffix(errOverlappingLocalRoot,
				fmt.Sprintf("'%s' overlaps with connection '%s' (%s)", localRoot, conn.name, conn.localRoot))
		}
	}

	return nil
}

// reassignDaemon re-keys a daemon after URL resolution (e.g. SSH config resolution
// may change the host). If another daemon already exists at the resolved key, the
// caller's daemon is superseded: d.supersededBy is set and the existing daemon is
// returned. Ref counts are NOT adjusted here; each caller that follows supersededBy
// must adjust ref counts itself (see followSupersede).
func (mgr *ConnectionManager) reassignDaemon(d *remoteDaemon, destURL *url.URL, identity string) *remoteDaemon {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	newKey := daemonKey(destURL, identity)

	// Already keyed correctly.
	if d.mapKey == newKey {
		return d
	}

	// Check if another daemon already exists at the resolved key.
	if existing, ok := mgr.daemons[newKey]; ok {
		// Supersede: remove the duplicate daemon from the map.
		delete(mgr.daemons, d.mapKey)
		d.supersededBy = existing

		// Close the duplicate daemon's transport. It was never used to
		// install a remote daemon, so Close is sufficient.
		go func() {
			if err := d.connector.Close(); err != nil {
				slog.Error("error closing duplicate connector during daemon supersede", "error", err)
			}
		}()

		return existing
	}

	delete(mgr.daemons, d.mapKey)
	d.mapKey = newKey
	mgr.daemons[newKey] = d

	return d
}

// followSupersede transfers a single ref count from the superseded daemon to the target.
// Must be called by each goroutine that follows a supersededBy pointer.
func (mgr *ConnectionManager) followSupersede(from, to *remoteDaemon) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	from.refCount--
	to.refCount++
}

// getOrCreateDaemon returns a shared remoteDaemon for the given host+identity,
// creating one if it doesn't exist. Must be called under connMgrMu.
func (mgr *ConnectionManager) getOrCreateDaemon(
	ctx context.Context,
	destURL *url.URL,
	identity string,
	scheme ConnectorFactory,
) (*remoteDaemon, error) {
	key := daemonKey(destURL, identity)

	if d, ok := mgr.daemons[key]; ok {
		d.refCount++

		return d, nil
	}

	connector, err := scheme.CreateConnector(ctx, destURL, identity)
	if err != nil {
		return nil, errors.Wrap(err)
	}

	d := newRemoteDaemon(connector)
	d.runCtx = mgr.runCtx
	d.mapKey = key
	d.refCount = 1
	mgr.daemons[key] = d

	return d, nil
}

// daemonKey returns a canonical key for a remote daemon based on the destination
// URL and identity. Two connections to the same user@host with the same identity
// share a daemon. The key normalizes away scheme differences and default ports.
func daemonKey(destURL *url.URL, identity string) string {
	user := ""
	if destURL.User != nil {
		user = destURL.User.Username()
	}

	host := destURL.Hostname()
	port := destURL.Port()

	// Strip default SSH port.
	if port == "22" {
		port = ""
	}

	hostPart := host
	if port != "" {
		hostPart = host + ":" + port
	}

	if user != "" {
		hostPart = user + "@" + hostPart
	}

	return hostPart + "::" + identity
}

var errUnknownScheme = errors.NewBare("unknown scheme")

func (mgr *ConnectionManager) scheme(destURL *url.URL) (ConnectorFactory, error) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	scheme, ok := mgr.schemes[destURL.Scheme]
	if !ok {
		return nil, errors.WrapSuffix(errUnknownScheme, destURL.Scheme)
	}

	return scheme, nil
}

// updateDaemonRemoteRoots refreshes the remote root directories stored on a daemon
// by scanning all connections that use it. Must be called with connMgrMu held.
func (mgr *ConnectionManager) updateDaemonRemoteRoots(d *remoteDaemon) {
	var roots []string

	for _, conn := range mgr.connections {
		if conn.lockedDaemon() == d {
			if root := conn.RemoteRoot(); root != "" {
				roots = append(roots, root)
			}
		}
	}

	d.remoteRoots.Store(&roots)
}

// Connections returns a snapshot of all connections, safe for concurrent iteration.
func (mgr *ConnectionManager) Connections() map[string]*Connection {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	snapshot := make(map[string]*Connection, len(mgr.connections))
	maps.Copy(snapshot, mgr.connections)

	return snapshot
}

// Connection returns an existing connection so long as it's in a valid state.
func (mgr *ConnectionManager) Connection(name string) (*Connection, error) {
	return mgr.connection(name, true)
}

// initialize sets up a new connection with a daemon running at the given destination.
// The connection is created immediately so it appears in Connections() during initialization,
// then the daemon is initialized. On failure the connection is cleaned up or left in Failed
// state depending on destroyIfFail.
func (mgr *ConnectionManager) initialize(
	ctx context.Context,
	name string,
	destURL *url.URL,
	localRoot string,
	remoteRoot string,
	identity string,
	destroyIfFail bool,
	background bool,
) (*Connection, error) {
	scheme, err := mgr.scheme(destURL)
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = destURL.Host
		slog.DebugContext(ctx, "no name provided; using host", "name", name)
	}

	daemon, existingConn, err := mgr.getOrCreateDaemonForConnection(ctx, name, destURL, identity, scheme)
	if err != nil {
		return nil, err
	}

	if existingConn != nil {
		return existingConn, nil
	}

	// Create the connection early so it is visible in Connections() during initialization.
	conn, err := mgr.createConnection(name, localRoot, remoteRoot, daemon, background)
	if err != nil {
		return nil, err
	}

	// Initialize the daemon. The first caller does the work; concurrent callers
	// block until initialization completes. Already-Connected daemons return immediately.
	initErr := daemon.Initialize(ctx, func() error {
		// After transport is established (hostnames resolved), re-key the daemon.
		// If another daemon already exists at the resolved key (e.g. two connections
		// to the same host restored in parallel), this daemon is superseded.
		newDaemon := mgr.reassignDaemon(daemon, destURL, identity)
		if newDaemon != daemon {
			return errDaemonSuperseded
		}

		return nil
	})

	if errors.Is(initErr, errDaemonSuperseded) {
		merged := daemon.supersededBy
		mgr.followSupersede(daemon, merged)
		conn.updateDaemon(merged)
	} else if initErr != nil {
		if destroyIfFail {
			mgr.connMgrMu.Lock()
			delete(mgr.connections, name)
			mgr.releaseDaemon(daemon)
			mgr.connMgrMu.Unlock()
		}

		return nil, errors.WrapPrefix(initErr, "error initializing connection")
	}

	return conn, nil
}

// Initialize sets up a new connection with a daemon running at the given destination.
func (mgr *ConnectionManager) Initialize(
	ctx context.Context, name string, destURL *url.URL, localRoot, remoteRoot, identity string, background bool,
) (*Connection, error) {
	return mgr.initialize(ctx, name, destURL, localRoot, remoteRoot, identity, true, background)
}

// Restore ensures a connection is re-established. It's similar to Initialize but will only ensure the connection's
// daemon is running.
func (mgr *ConnectionManager) Restore(
	ctx context.Context, name string, destURL *url.URL, localRoot, remoteRoot, identity string, background bool,
) (*Connection, error) {
	return mgr.initialize(ctx, name, destURL, localRoot, remoteRoot, identity, false, background)
}

var (
	errConnectionNotFound         = errors.NewBare("connection not found")
	errCannotUseConnectionInState = errors.NewBare("cannot use connection in current state")
)

func (mgr *ConnectionManager) connection(name string, safely bool) (*Connection, error) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	conn, ok := mgr.connections[name]
	if !ok {
		return nil, errors.WrapSuffix(errConnectionNotFound, name)
	}

	if safely {
		if state, _ := conn.State(); state != ConnectionStateConnected {
			return nil, errors.WrapSuffix(errCannotUseConnectionInState, state.String())
		}
	}

	return conn, nil
}

// UpdateForwardCommands updates the commands to forward for the given connection.
func (mgr *ConnectionManager) UpdateForwardCommands(name string, commands []ForwardCommandIntent) error {
	conn, err := mgr.Connection(name)
	if err != nil {
		return err
	}

	conn.UpdateForwardCommands(commands)

	return nil
}

// RemoveForwardCommands removes the named commands from the given connection's forward list.
func (mgr *ConnectionManager) RemoveForwardCommands(name string, commands []string) error {
	conn, err := mgr.Connection(name)
	if err != nil {
		return err
	}

	conn.RemoveForwardCommands(commands)

	return nil
}

// Close ends our sessions with any existing connection and closes daemons.
func (mgr *ConnectionManager) Close() {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	mgr.runCtxCancel()

	for _, conn := range mgr.connections {
		conn.Close()
	}

	for key, d := range mgr.daemons {
		if err := d.Close(); err != nil {
			slog.Error("error closing daemon", "error", err)
		}

		delete(mgr.daemons, key)
	}

	if mgr.connectionRootsPath != "" {
		os.Remove(mgr.connectionRootsPath)
	}
}

// Run periodically checks in and reports on all connections.
func (mgr *ConnectionManager) Run(runCtx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-runCtx.Done():
			err := context.Cause(runCtx)
			if err != nil {
				if errors.Is(err, ErrShuttingDown) {
					slog.DebugContext(runCtx, "stopping connection manager")
				} else {
					slog.ErrorContext(runCtx, "stopping connection manager", "error", context.Cause(runCtx))
				}
			}

			return
		}

		mgr.tick(runCtx)
	}
}

func (mgr *ConnectionManager) tick(ctx context.Context) {
	mgr.checkAndLogDaemons(ctx)
	mgr.RefreshConnectionRootsFile()
}

// checkAndLogDaemons health-checks each daemon (one Status RPC per daemon)
// and logs connection state changes using the cached responses.
func (mgr *ConnectionManager) checkAndLogDaemons(ctx context.Context) {
	mgr.connMgrMu.Lock()

	daemonSnapshot := make([]*remoteDaemon, 0, len(mgr.daemons))
	for _, d := range mgr.daemons {
		daemonSnapshot = append(daemonSnapshot, d)
	}

	connSnapshot := make([]*Connection, 0, len(mgr.connections))
	for _, conn := range mgr.connections {
		connSnapshot = append(connSnapshot, conn)
	}

	mgr.connMgrMu.Unlock()

	// Health check each daemon and cache the status response.
	daemonStatuses := make(map[*remoteDaemon]*graftv1.StatusResponse, len(daemonSnapshot))

	for _, d := range daemonSnapshot {
		resp := mgr.checkDaemon(ctx, d)
		if resp != nil {
			daemonStatuses[d] = resp
		}
	}

	// Log connection states using cached daemon status.
	for _, conn := range connSnapshot {
		state, _ := conn.State()
		if state == ConnectionStateConnected {
			resp := daemonStatuses[conn.lockedDaemon()]
			if resp == nil {
				continue
			}

			if _, changed := conn.Hash(resp); changed {
				logArgs := []any{
					"name", conn.Name(),
					"state", state,
				}
				logArgs = append(logArgs, conn.StateFields()...)
				slog.DebugContext(ctx, "conn info", logArgs...)
				slog.DebugContext(ctx, "system status",
					"name", conn.Name(),
					"healthy", resp.GetHealthy(),
					"version_info", resp.GetVersionInfo(),
					"uptime", resp.GetUptime().AsDuration().String(),
					"recent_logs", resp.GetRecentLogs(),
				)
			}
		} else {
			if _, changed := conn.Hash(nil); changed {
				slog.DebugContext(ctx, "conn info", "name", conn.Name(), "state", state)
			}
		}
	}
}

func (mgr *ConnectionManager) checkDaemon(ctx context.Context, d *remoteDaemon) *graftv1.StatusResponse {
	state, _ := d.State()
	if state == ConnectionStateFailed {
		slog.InfoContext(ctx, "retrying failed connection", "destination", d.Destination())

		go d.Reconnect(d.runCtx)

		return nil
	}

	if state != ConnectionStateConnected {
		return nil
	}

	cc := d.RemoteClientConn()
	if cc == nil {
		return nil
	}

	remClient := graftv1.NewGraftServiceClient(cc)

	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second) //nolint:mnd // health check timeout
	defer healthCancel()

	resp, err := remClient.Status(healthCtx, &graftv1.StatusRequest{})
	if err != nil {
		slog.ErrorContext(ctx, "health check failed; triggering reconnect",
			"error", err, "destination", d.Destination())

		go d.Reconnect(d.runCtx)

		return nil
	}

	return resp
}

// EstablishSynchronization sets up bidi file sync between this local host and a remote connection configured by
// the intent.
func (mgr *ConnectionManager) EstablishSynchronization(
	ctx context.Context,
	name string,
	syncIntent SynchronizationIntent,
	syncManager *synchronization.Manager,
	syncProtoNum int,
) error {
	conn, err := mgr.Connection(name)
	if err != nil {
		return err
	}

	if err := conn.EstablishSynchronization(ctx, syncIntent, syncManager, syncProtoNum); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

// Remove removes the named connection. Depending on the scheme, the daemon may be destroyed as well.
func (mgr *ConnectionManager) Remove(ctx context.Context, name string) error {
	return mgr.remove(ctx, name, false)
}

func (mgr *ConnectionManager) remove(ctx context.Context, name string, safely bool) error {
	conn, err := mgr.connection(name, safely)
	if err != nil {
		return err
	}

	if err := conn.Close(); err != nil {
		slog.ErrorContext(ctx, "error closing connection; still removing from records", "error", err)
	}

	mgr.connMgrMu.Lock()

	delete(mgr.connections, name)
	mgr.updateDaemonRemoteRoots(conn.daemon)
	mgr.writeConnectionRootsFile()

	// Decrement daemon ref count and destroy if last connection.
	mgr.releaseDaemon(conn.daemon)

	mgr.connMgrMu.Unlock()

	return nil
}

// releaseDaemon decrements a daemon's refCount and destroys it when it reaches zero.
// Must be called under connMgrMu.
func (mgr *ConnectionManager) releaseDaemon(d *remoteDaemon) {
	d.refCount--

	if d.refCount > 0 {
		return
	}

	delete(mgr.daemons, d.mapKey)

	// Destroy asynchronously to avoid holding the lock during I/O.
	// Use the manager's runCtx rather than the request context so the
	// teardown isn't cancelled when the RPC returns.
	go func() {
		if err := d.Destroy(mgr.runCtx); err != nil {
			slog.ErrorContext(mgr.runCtx, "error destroying daemon", "error", err)
		}
	}()
}

// writeConnectionRootsFile writes a global file mapping local roots to connection names.
// This allows new shell sessions to resolve connection names from CWD before the
// per-session file is written by the daemon's reconciliation tick.
//
// Assumes connMgrMu is held.
func (mgr *ConnectionManager) writeConnectionRootsFile() {
	if mgr.connectionRootsPath == "" {
		return
	}

	var buf strings.Builder
	// Sort for deterministic output so the skip-if-unchanged check works.
	names := make([]string, 0, len(mgr.connections))
	for name := range mgr.connections {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		conn := mgr.connections[name]

		if conn.background {
			continue
		}

		localRoot := conn.localRoot
		if localRoot == "" {
			continue
		}

		if resolved, err := filepath.EvalSymlinks(localRoot); err == nil {
			localRoot = resolved
		}

		if abs, err := filepath.Abs(localRoot); err == nil {
			localRoot = abs
		}

		fmt.Fprintf(&buf, "%s\t%s\n", localRoot, name)
	}

	content := buf.String()

	existing, err := os.ReadFile(mgr.connectionRootsPath)
	if err == nil && string(existing) == content {
		return
	}

	if err := os.WriteFile(mgr.connectionRootsPath, []byte(content), FilePerms); err != nil {
		slog.Error("error writing connection roots file", "error", err)
	}
}

// RefreshConnectionRootsFile re-writes the global connection_roots file.
func (mgr *ConnectionManager) RefreshConnectionRootsFile() {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	mgr.writeConnectionRootsFile()
}

func (mgr *ConnectionManager) connectionByCWD(ctx context.Context, cwd string) (*Connection, bool) {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	return mgr.matchConnectionByCWD(ctx, cwd)
}

func (mgr *ConnectionManager) matchConnectionByCWD(ctx context.Context, cwd string) (*Connection, bool) {
	if cwd == "" {
		return nil, false
	}

	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		slog.DebugContext(ctx, "error calling EvalSymlinks", "path", cwd, "error", err)

		return nil, false
	}

	cwd, err = filepath.Abs(cwd)
	if err != nil {
		slog.DebugContext(ctx, "error calling Abs", "path", cwd, "error", err)

		return nil, false
	}

	cwd = strings.ToLower(cwd)

	matches := make([]*Connection, 0, len(mgr.connections))

	for _, conn := range mgr.connections {
		if conn.background {
			continue
		}

		localRoot := conn.LocalRoot()
		if localRoot == "" {
			continue
		}

		localRoot, err := filepath.EvalSymlinks(localRoot)
		if err != nil {
			slog.DebugContext(ctx, "error calling EvalSymlinks", "path", localRoot, "error", err)

			continue
		}

		localRoot, err = filepath.Abs(localRoot)
		if err != nil {
			slog.DebugContext(ctx, "error calling Abs", "path", localRoot, "error", err)

			continue
		}

		localRoot = strings.ToLower(localRoot)

		if _, ok := hasPathPrefix(cwd, localRoot); !ok {
			continue
		}

		matches = append(matches, conn)
	}

	if len(matches) == 1 {
		return matches[0], true
	}

	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name()
		}

		slog.WarnContext(ctx, "multiple connections match cwd; cannot auto-select",
			"cwd", cwd,
			"connections", names)

		return nil, false
	}

	return nil, false
}

func (mgr *ConnectionManager) forwardings(_ context.Context, selectedConn *Connection) map[ForwardCommandIntent][]string {
	mgr.connMgrMu.Lock()
	defer mgr.connMgrMu.Unlock()

	flatFwds := map[ForwardCommandIntent][]string{}

	for destName, conn := range mgr.connections {
		for _, fwd := range conn.ForwardIntents() {
			// check if forwards are for this current session's connection
			if !fwd.Global && (selectedConn == nil || selectedConn != conn) {
				continue
			}

			if !fwd.Prefix {
				if otherDest, ok := flatFwds[fwd]; ok {
					slog.Error(
						"conflicting forwards",
						"name", fwd.Name,
						"destinations", []string{destName, strings.Join(otherDest, ", ")})

					continue
				}
			}

			flatFwds[fwd] = append(flatFwds[fwd], destName)
		}
	}

	return flatFwds
}

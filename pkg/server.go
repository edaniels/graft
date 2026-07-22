package graft

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	urlpkg "github.com/mutagen-io/mutagen/pkg/url"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// A Server in graft is what implements the GraftService monolith.
//
// TODO(erd): Consider splitting the server into a gRPC transport layer and a services layer.
type Server struct {
	graftv1.UnimplementedGraftServiceServer

	//nolint:containedctx // self-owned
	runCtx                 context.Context
	role                   ServerRole
	synchronizationManager *synchronization.Manager
	rootConfig             *RootConfig
	connMgr                *ConnectionManager
	sessMgr                *SessionManager
	grpcSrv                *grpc.Server
	oobListeners           map[*oobListener]struct{}
	rootConfigPath         string
	sockPath               string
	pidPath                string
	envProviders           *EnvProviderSet
	activeWorkers          sync.WaitGroup
	serverMu               sync.Mutex
	oobListenersMu         sync.Mutex
	closed                 bool
	restartRequested       bool
	buffLineWriter         *BufferedLineWriter

	identity        string
	sshAuthSockPath string
	startedAt       time.Time
	lastOrphanReap  time.Time
}

// NewServer returns a new daemon capable of serving any server role. All configuration is specified
// by the given config.
func NewServer(
	config *RootConfig,
	role ServerRole,
	rootConfigPath string,
	replace bool,
	buffLineWriter *BufferedLineWriter,
	identity string,
	logLevel slog.Level,
) (*Server, error) {
	var (
		connMgr      *ConnectionManager
		sessMgr      *SessionManager
		envProviders *EnvProviderSet
	)

	switch role {
	case ServerRoleLocal:
		connMgr = NewConnectionManager(logLevel)

		connMgr.RegisterConnectorFactory(dockerSchemeName, newDockerConnectorFactory())

		connMgr.RegisterConnectorFactory(sshSchemeName, newSSHConnectorFactory())

		var err error

		sessMgr, err = NewSessionManager(connMgr)
		if err != nil {
			return nil, err
		}
	case ServerRoleRemote:
		// Remote daemons track ports opened by commands they run.
		// Remote daemons detect directory-aware env managers (e.g. mise).
		envProviders = NewEnvProviderSet()
	default:
		return nil, errors.WrapSuffix(errUnknownServerRole, role.String())
	}

	// Setup socket for client/server communication. It's also kind of a process lock.
	// For remote daemons with an identity, namespace the socket path.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	// Use a graft-namespaced mutagen data dir. On local, NewManager reloads
	// paused sessions from here so ancestor archives survive daemon restarts.
	// On remote, mutagen's endpoint writes its cache and staging here;
	// identity scoping keeps multiple remote daemons on one machine from
	// colliding.
	syncStateDir, syncDirErr := graftSyncDir(homeDir, role, identity)
	if syncDirErr != nil {
		return nil, syncDirErr
	}

	if mkErr := os.MkdirAll(syncStateDir, DirPerms); mkErr != nil {
		return nil, errors.Wrap(mkErr)
	}

	if setErr := os.Setenv("MUTAGEN_DATA_DIRECTORY", syncStateDir); setErr != nil {
		return nil, errors.Wrap(setErr)
	}

	// Remote daemons don't own sync sessions, so the non-persistent manager
	// is fine. They still need MUTAGEN_DATA_DIRECTORY set above for the
	// remote endpoint's cache and staging.
	var synchronizationManager *synchronization.Manager

	if role == ServerRoleLocal {
		synchronizationManager, err = synchronization.NewManager(logging.NewLoggerOnSlogger(slog.Default()))
	} else {
		synchronizationManager, err = synchronization.NewManagerWithoutPersistence(logging.NewLoggerOnSlogger(slog.Default()))
	}

	if err != nil {
		return nil, errors.Wrap(err)
	}

	var sockIdentity string
	if role == ServerRoleRemote && identity != "" {
		sockIdentity = identity
	}

	sockPath, err := daemonSocketPath(graftStateHome(homeDir), role, sockIdentity)
	if err != nil {
		return nil, err
	}

	sockDir := filepath.Dir(sockPath)
	pidPath := filepath.Join(sockDir, "graftd.pid")

	if err := os.MkdirAll(sockDir, DirPerms); err != nil {
		return nil, errors.Wrap(err)
	}

	if replace {
		killDaemonByPIDFile(pidPath)

		// The killed daemon's Close() may have already removed the socket.
		if removeErr := os.Remove(sockPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return nil, errors.Wrap(removeErr)
		}
	} else if _, err := os.Stat(sockPath); err == nil {
		argsClone := slices.Clone(os.Args)
		argsClone = append(argsClone, "--replace")

		return nil, errors.New(
			"daemon already running on " + sockPath + " or didn't cleanly shutdown (use " +
				strings.Join(argsClone, " ") + " if needed)")
	}

	server := &Server{
		role:           role,
		identity:       identity,
		connMgr:        connMgr,
		sessMgr:        sessMgr,
		envProviders:   envProviders,
		rootConfig:     config,
		rootConfigPath: rootConfigPath,
		sockPath:       sockPath,
		pidPath:        pidPath,

		synchronizationManager: synchronizationManager,

		oobListeners:   map[*oobListener]struct{}{},
		buffLineWriter: buffLineWriter,

		startedAt: time.Now(),
	}
	synchronization.ProtocolHandlers[urlpkg.Protocol(syncProtoNum)] = &mutagenSyncProtocolHandler{
		server: server,
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(server.OOBUnaryServerInterceptor))
	graftv1.RegisterGraftServiceServer(grpcServer, server)
	reflection.Register(grpcServer)
	server.grpcSrv = grpcServer

	return server, nil
}

// Run runs the server until it's requestesd to stop by either Close or runCtx.
func (srv *Server) Run(runCtx context.Context) error {
	srv.serverMu.Lock()

	if srv.closed {
		srv.serverMu.Unlock()

		return errors.New("server closed")
	}

	srv.runCtx = runCtx
	srv.serverMu.Unlock()

	if srv.role == ServerRoleLocal {
		srv.activeWorkers.Go(func() {
			srv.connMgr.Run(runCtx)
		})
		srv.activeWorkers.Go(func() {
			srv.sessMgr.Run(runCtx)
		})
		srv.activeWorkers.Go(func() {
			err := srv.restore(runCtx)
			if err != nil {
				if !IsCanceledError(err) {
					slog.ErrorContext(runCtx, "error restoring server", "error", err)
				}
			}
		})
		srv.activeWorkers.Go(func() {
			srv.reconcileLoop(runCtx)
		})
	}

	listener, err := listenUnixSocket(srv.sockPath)
	if err != nil {
		// TODO(erd): Listen failure may leave restore goroutine blocked indefinitely.
		return errors.Wrap(err)
	}

	if err := os.WriteFile(srv.pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		listener.Close()

		return errors.Wrap(err)
	}

	slog.InfoContext(runCtx, "serving", "address", listener.Addr().String())

	srv.activeWorkers.Go(func() {
		err := srv.grpcSrv.Serve(listener)
		if err != nil {
			slog.DebugContext(runCtx, "grpc server serve error", "error", err)
		}
	})

	return nil
}

// restore is a critical step of server startup. All connections specified in the config are reestablished here.
func (srv *Server) restore(runCtx context.Context) error {
	errs := make(chan error, len(srv.rootConfig.Connections))

	// restore each connection in parallel (no limit).
	for _, connConfig := range srv.rootConfig.Connections {
		go func(conf ConnectionConfig) {
			// TODO(erd): conditionally re-install
			slog.DebugContext(runCtx, "restoring", "name", conf.Name)

			destURL, err := ParseDestination(conf.Destination)
			if err != nil {
				errs <- err

				return
			}

			// TODO(erd): the restoration of this could probably be similar to initialization so that we don't
			// need to write duplicate code.
			_, err = srv.connMgr.Restore(
				runCtx, conf.Name, destURL, conf.LocalRoot, conf.RemoteRoot, srv.identity, conf.Background)
			if err != nil {
				slog.ErrorContext(runCtx, "error restoring connection", "name", conf.Name, "error", err)

				errs <- err

				return
			}

			var fwdIntents []ForwardCommandIntent
			for _, fwd := range conf.Forward {
				fwdIntents = append(fwdIntents, ForwardCommandIntent{Name: fwd, Prefix: false})
			}

			for _, fwd := range conf.PrefixForward {
				fwdIntents = append(fwdIntents, ForwardCommandIntent{Name: fwd, Prefix: true})
			}

			if err := srv.connMgr.UpdateForwardCommands(conf.Name, fwdIntents); err != nil {
				slog.ErrorContext(runCtx, "error updating forward commands for connection", "name", conf.Name, "error", err)

				errs <- err

				return
			}

			for _, syncIntent := range conf.Synchronizations {
				shadowed, err := srv.connMgr.EstablishSynchronization(
					runCtx,
					conf.Name,
					SynchronizationIntentFromConfig(syncIntent),
					srv.synchronizationManager,
				)
				if err != nil {
					slog.ErrorContext(runCtx, "error re-establishing synchronization for connection", "name", conf.Name, "error", err)

					errs <- err

					return
				}

				logShadowedSyncIncludes(runCtx, conf.Name, syncIntent.FromLocal, shadowed)
			}

			srv.restorePortForwards(runCtx, conf)

			slog.DebugContext(runCtx, "restored connection", "name", conf.Name)

			errs <- nil
		}(connConfig)
	}

	allErrs := make([]error, 0, len(srv.rootConfig.Connections))
	for range len(srv.rootConfig.Connections) {
		allErrs = append(allErrs, <-errs)
	}

	return errors.Join(allErrs...)
}

// reconcileInterval is how often the server reconciles desired connection state
// (from config) against the live state on each connection.
const reconcileInterval = time.Second

// orphanReapInterval is how often we scan for sync sessions to terminate.
// Slow because orphans only appear when a user removes an intent from config,
// and each reap walks the full session list under a lock.
const orphanReapInterval = 30 * time.Second

// reconcileLoop periodically re-establishes per-connection state (sync intents,
// forward commands, port forwards) that's declared in the config but not active
// on a Connected connection. This makes restoration resilient to: (a) initial
// Restore failing because the network isn't ready yet at user-login, and
// (b) the daemon reconnecting after a transport drop.
func (srv *Server) reconcileLoop(runCtx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
		}

		srv.reconcileConnections(runCtx)
	}
}

// reconcileConnections walks the configured connections and ensures each one's
// desired state is applied to its connection (when Connected). Errors are
// logged and the next tick will retry.
func (srv *Server) reconcileConnections(ctx context.Context) {
	srv.serverMu.Lock()
	pending := make([]ConnectionConfig, 0, len(srv.rootConfig.Connections))

	for _, connConfig := range srv.rootConfig.Connections {
		pending = append(pending, cloneConnectionConfig(connConfig))
	}

	srv.serverMu.Unlock()

	anyConnected := false

	for _, conf := range pending {
		if ctx.Err() != nil {
			return
		}

		conn, err := srv.connMgr.LookupConnection(conf.Name)
		if err != nil {
			continue
		}

		state, _ := conn.State()
		if state != ConnectionStateConnected {
			continue
		}

		anyConnected = true

		srv.applyConnectionSpec(ctx, conn, conf)
	}

	// Reap orphan sync sessions. Gated on at least one Connected connection
	// so we don't terminate everything during the startup window where no
	// connection has come up yet.
	if anyConnected && time.Since(srv.lastOrphanReap) >= orphanReapInterval {
		srv.reapOrphanSyncs(ctx, pending)
		srv.lastOrphanReap = time.Now()
	}
}

// applyConnectionSpec ensures the live state of conn matches the desired state
// expressed by conf. New connection-level features (env vars, watches, etc.)
// should add a step here rather than wiring themselves into both the startup
// restore path and the reconcile loop. Each step must itself be idempotent.
//
// Callers are responsible for ensuring conn is in a state where remote calls
// are safe (typically ConnectionStateConnected).
func (srv *Server) applyConnectionSpec(ctx context.Context, conn *Connection, conf ConnectionConfig) {
	srv.reconcileForwardCommands(ctx, conn, conf)
	srv.reconcileSyncs(ctx, conn, conf)
	srv.reconcilePortForwards(ctx, conn, conf)
}

// cloneConnectionConfig returns a copy of conf safe to use after dropping the
// server lock. ConnectionConfig contains slices of value-type elements, so a
// per-slice Clone is a sufficient deep copy.
func cloneConnectionConfig(conf ConnectionConfig) ConnectionConfig {
	conf.Forward = slices.Clone(conf.Forward)
	conf.PrefixForward = slices.Clone(conf.PrefixForward)
	conf.Synchronizations = slices.Clone(conf.Synchronizations)
	conf.Ports = slices.Clone(conf.Ports)

	return conf
}

// reapOrphanSyncs terminates graft-owned sync sessions whose name doesn't
// match any intent in config. The only Terminate path; every other lifecycle
// step (network drop, daemon restart, disconnect) pauses instead.
func (srv *Server) reapOrphanSyncs(ctx context.Context, pending []ConnectionConfig) {
	if srv.synchronizationManager == nil {
		return
	}

	expected := expectedSyncSessionNames(pending)

	_, states, err := srv.synchronizationManager.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		slog.WarnContext(ctx, "reconcile: error listing sync sessions for orphan reap", "error", err)

		return
	}

	for _, state := range states {
		name := state.GetSession().GetName()
		if !isGraftSyncName(name) {
			continue
		}

		if expected[name] {
			continue
		}

		id := state.GetSession().GetIdentifier()
		if err := srv.synchronizationManager.Terminate(
			ctx, &selection.Selection{Specifications: []string{id}}, "",
		); err != nil {
			slog.WarnContext(ctx, "reconcile: error terminating orphan sync",
				"session_id", id, "name", name, "error", err)

			continue
		}

		slog.InfoContext(ctx, "reconcile: terminated orphan sync", "session_id", id, "name", name)
	}
}

func (srv *Server) reconcileSyncs(ctx context.Context, conn *Connection, conf ConnectionConfig) {
	for _, intent := range computeMissingSyncs(conf.Synchronizations, conn.Synchronizations()) {
		shadowed, err := srv.connMgr.EstablishSynchronization(
			ctx, conf.Name, intent, srv.synchronizationManager,
		)
		if err != nil {
			slog.WarnContext(ctx, "reconcile: error establishing synchronization",
				"name", conf.Name, "from", intent.FromLocal, "to", intent.ToRemote, "error", err)

			continue
		}

		logShadowedSyncIncludes(ctx, conf.Name, intent.FromLocal, shadowed)

		slog.InfoContext(ctx, "reconcile: established synchronization",
			"name", conf.Name, "from", intent.FromLocal, "to", intent.ToRemote)
	}
}

// logShadowedSyncIncludes logs any syncInclude patterns that will not take
// effect. Config-driven establishment (restore/reconcile) has no client to
// return to, so the warning goes to the daemon log; the CLI path surfaces the
// same warnings directly to the user via the RPC response.
func logShadowedSyncIncludes(ctx context.Context, connName, fromLocal string, shadowed []string) {
	for _, pattern := range shadowed {
		slog.WarnContext(ctx, "syncInclude pattern is shadowed by a directory-level ignore and will not sync; "+
			"ignore the directory's contents (e.g. \"dir/**\") instead of the directory itself",
			"name", connName, "from", fromLocal, "pattern", pattern)
	}
}

func (srv *Server) reconcileForwardCommands(ctx context.Context, conn *Connection, conf ConnectionConfig) {
	desired := make([]ForwardCommandIntent, 0, len(conf.Forward)+len(conf.PrefixForward))
	for _, name := range conf.Forward {
		desired = append(desired, ForwardCommandIntent{Name: name, Prefix: false})
	}

	for _, name := range conf.PrefixForward {
		desired = append(desired, ForwardCommandIntent{Name: name, Prefix: true})
	}

	missing := computeMissingForwardCommands(desired, conn.ForwardIntents())
	if len(missing) == 0 {
		return
	}

	conn.UpdateForwardCommands(missing)
	slog.InfoContext(ctx, "reconcile: added forward commands", "name", conf.Name, "count", len(missing))
}

func (srv *Server) reconcilePortForwards(ctx context.Context, conn *Connection, conf ConnectionConfig) {
	if len(conf.Ports) == 0 {
		return
	}

	daemon := conn.lockedDaemon()

	for _, portSpec := range conf.Ports {
		parsed, parseErr := ParsePortSpec(portSpec)
		if parseErr != nil {
			continue
		}

		// AddExplicitPortForward is idempotent (no-op when the spec is already
		// recorded as explicit), so it's safe to call every tick.
		if err := daemon.AddExplicitPortForward(ctx, parsed); err != nil {
			slog.WarnContext(ctx, "reconcile: error establishing port forward",
				"name", conf.Name, "spec", portSpec, "error", err)
		}
	}
}

// computeMissingSyncs returns the desired sync intents that are not already
// represented in active by an exact (FromLocal, ToRemote) match.
func computeMissingSyncs(desired []SynchronizationIntentConfig, active []SynchronizationIntent) []SynchronizationIntent {
	if len(desired) == 0 {
		return nil
	}

	activeByLocal := make(map[string]SynchronizationIntent, len(active))
	for _, a := range active {
		activeByLocal[a.FromLocal] = a
	}

	missing := make([]SynchronizationIntent, 0, len(desired))

	for _, d := range desired {
		intent := SynchronizationIntentFromConfig(d)
		if existing, ok := activeByLocal[intent.FromLocal]; ok &&
			existing.ToRemote == intent.ToRemote && existing.SyncGit == intent.SyncGit &&
			syncModesCompatible(existing.DefaultFileMode, existing.DefaultDirectoryMode,
				intent.DefaultFileMode, intent.DefaultDirectoryMode) &&
			syncIncludesCompatible(existing.SyncInclude, intent.SyncInclude) {
			continue
		}

		missing = append(missing, intent)
	}

	return missing
}

// expectedSyncSessionNames returns the session names implied by the given
// configs: one per synchronization, plus a .git replica name for each
// synchronization that enables SyncGit. The orphan reaper terminates any
// graft-owned session not in this set.
func expectedSyncSessionNames(pending []ConnectionConfig) map[string]bool {
	expected := make(map[string]bool)

	for _, conf := range pending {
		for _, s := range conf.Synchronizations {
			intent := SynchronizationIntentFromConfig(s)
			expected[syncSessionName(conf.Name, intent)] = true

			if intent.SyncGit {
				expected[syncSessionName(conf.Name, gitReplicaIntent(intent))] = true
			}
		}
	}

	return expected
}

// computeMissingForwardCommands returns the desired forward intents not already
// present in active (matched by exact (Name, Prefix) equality).
func computeMissingForwardCommands(desired, active []ForwardCommandIntent) []ForwardCommandIntent {
	if len(desired) == 0 {
		return nil
	}

	activeSet := make(map[ForwardCommandIntent]struct{}, len(active))
	for _, a := range active {
		activeSet[a] = struct{}{}
	}

	missing := make([]ForwardCommandIntent, 0, len(desired))

	for _, d := range desired {
		if _, ok := activeSet[d]; ok {
			continue
		}

		missing = append(missing, d)
	}

	return missing
}

func (srv *Server) restorePortForwards(ctx context.Context, conf ConnectionConfig) {
	if len(conf.Ports) == 0 {
		return
	}

	conn, err := srv.connMgr.Connection(conf.Name)
	if err != nil {
		slog.ErrorContext(ctx, "error getting connection for port restore",
			"name", conf.Name, "error", err)

		return
	}

	daemon := conn.lockedDaemon()

	for _, portSpec := range conf.Ports {
		parsed, parseErr := ParsePortSpec(portSpec)
		if parseErr != nil {
			slog.ErrorContext(ctx, "invalid port spec in config, skipping",
				"name", conf.Name, "spec", portSpec, "error", parseErr)

			continue
		}

		if fwdErr := daemon.AddExplicitPortForward(ctx, parsed); fwdErr != nil {
			slog.ErrorContext(ctx, "error restoring explicit port forward",
				"name", conf.Name, "spec", portSpec, "error", fwdErr)
		}
	}
}

func (srv *Server) persistConfig() {
	if err := srv.rootConfig.Persist(srv.rootConfigPath); err != nil {
		slog.Error("error persisting config", "error", err)
	}
}

// UpdateForwardCommands updates the commands to forward for the connection associated with the destination label and
// then persists them; if an error happens updating, the config is not persisted.
func (srv *Server) UpdateForwardCommands(commands []ForwardCommandIntent, toDestination string) error {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(toDestination)
	if err != nil {
		return err
	}

	if err := srv.connMgr.UpdateForwardCommands(conn.Name(), commands); err != nil {
		return err
	}

	for idx, connConfig := range srv.rootConfig.Connections {
		if connConfig.Name == conn.Name() {
			for _, cmd := range commands {
				// TODO(erd): Prevent duplicate command entries; currently appends without dedup.
				if cmd.Prefix {
					connConfig.PrefixForward = append(connConfig.PrefixForward, cmd.Name)

					continue
				}

				connConfig.Forward = append(connConfig.Forward, cmd.Name)
			}

			srv.rootConfig.Connections[idx] = connConfig

			break
		}
	}

	srv.persistConfig()

	return nil
}

// RemoveForwardCommands removes the specified commands from the connection associated with the
// destination label and then persists the config.
func (srv *Server) RemoveForwardCommands(commands []string, fromDestination string) error {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(fromDestination)
	if err != nil {
		return err
	}

	if err := srv.connMgr.RemoveForwardCommands(conn.Name(), commands); err != nil {
		return err
	}

	toRemove := make(map[string]struct{}, len(commands))
	for _, cmd := range commands {
		toRemove[cmd] = struct{}{}
	}

	for idx, connConfig := range srv.rootConfig.Connections {
		if connConfig.Name == conn.Name() {
			connConfig.Forward = slices.DeleteFunc(connConfig.Forward, func(name string) bool {
				_, remove := toRemove[name]

				return remove
			})
			connConfig.PrefixForward = slices.DeleteFunc(connConfig.PrefixForward, func(name string) bool {
				_, remove := toRemove[name]

				return remove
			})
			srv.rootConfig.Connections[idx] = connConfig

			break
		}
	}

	srv.persistConfig()

	return nil
}

// UpdateSynchronizations updates the connection associated with the destination label with the latest
// synchronization info.
//
// TODO(erd): this method seems a little weird compared to UpdateForwardCommands considering it looks at
// the connection for state instead of providing it state.
func (srv *Server) UpdateSynchronizations(forConn string) error {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(forConn)
	if err != nil {
		return err
	}

	for idx, connConfig := range srv.rootConfig.Connections {
		if connConfig.Name == conn.Name() {
			intents := conn.Synchronizations()

			intentConfigs := make([]SynchronizationIntentConfig, 0, len(intents))
			for _, intent := range conn.Synchronizations() {
				intentConfigs = append(intentConfigs, intent.AsConfig())
			}

			connConfig.Synchronizations = intentConfigs
			srv.rootConfig.Connections[idx] = connConfig

			break
		}
	}

	srv.persistConfig()

	return nil
}

// Close shuts down each service and removes the daemon socket path for future
// daemons to set up.
func (srv *Server) Close() {
	srv.serverMu.Lock()

	if srv.closed {
		srv.serverMu.Unlock()

		return
	}

	srv.closed = true
	defer os.Remove(srv.sockPath)
	defer os.Remove(srv.pidPath)

	if srv.runCtx == nil {
		return
	}

	srv.serverMu.Unlock()

	// Close connections before Shutdown. Shutdown halts the controllers,
	// after which Pause fails with "controller disabled" and never flips the
	// paused flag the next startup needs.
	if srv.connMgr != nil {
		srv.connMgr.Close()
	}

	srv.synchronizationManager.Shutdown()

	if srv.sessMgr != nil {
		srv.sessMgr.Close()
	}

	srv.grpcSrv.Stop()
	srv.activeWorkers.Wait()
	slog.Info("cleanly shutdown")
}

// A ServerRole identifies a server as either a local or remote daemon.
type ServerRole int

const (
	// ServerRoleLocal is for a local daemon where a client lives.
	ServerRoleLocal ServerRole = iota
	// ServerRoleRemote is for a remote daemon where a server lives that accepts
	// commands/file synchronization.
	ServerRoleRemote
)

var errUnknownServerRole = errors.NewBare("unknown server role")

func (r ServerRole) String() string {
	switch r {
	case ServerRoleLocal:
		return "local"
	case ServerRoleRemote:
		return "remote"
	default:
		return "<unknown>"
	}
}

// RestartRequested returns whether or not the server has been asked to restart.
func (srv *Server) RestartRequested() bool {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	return srv.restartRequested
}

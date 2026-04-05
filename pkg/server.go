package graft

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mutagen-io/mutagen/pkg/logging"
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
	syncProtoNum           int
	serverMu               sync.Mutex
	oobListenersMu         sync.Mutex
	closed                 bool
	restartRequested       bool
	buffLineWriter         *BufferedLineWriter

	identity        string
	sshAuthSockPath string
	startedAt       time.Time
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

	synchronizationManager, err := synchronization.NewManagerWithoutPersistence(logging.NewLoggerOnSlogger(slog.Default()))
	if err != nil {
		return nil, errors.Wrap(err)
	}

	// Setup socket for client/server communication. It's also kind of a process lock.
	// For remote daemons with an identity, namespace the socket path.
	homeDir, err := os.UserHomeDir()
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

		// Mutagen stuff
		syncProtoNum:           int(nextSyncProtoNum.Add(1)),
		synchronizationManager: synchronizationManager,

		oobListeners:   map[*oobListener]struct{}{},
		buffLineWriter: buffLineWriter,

		startedAt: time.Now(),
	}
	synchronization.ProtocolHandlers[urlpkg.Protocol(server.syncProtoNum)] = &mutagenSyncProtocolHandler{ //nolint:gosec // overflow okay
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
	}

	listener, err := net.Listen("unix", srv.sockPath) //nolint:noctx
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
				err := srv.connMgr.EstablishSynchronization(
					runCtx,
					conf.Name,
					SynchronizationIntentFromConfig(syncIntent),
					srv.synchronizationManager,
					srv.syncProtoNum,
				)
				if err != nil {
					slog.ErrorContext(runCtx, "error re-establishing synchronization for connection", "name", conf.Name, "error", err)

					errs <- err

					return
				}
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

	srv.synchronizationManager.Shutdown()

	if srv.sessMgr != nil {
		srv.sessMgr.Close()
	}

	if srv.connMgr != nil {
		srv.connMgr.Close()
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

package graft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/selection"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/endpoint/remote"
	// register protocol.
	_ "github.com/mutagen-io/mutagen/pkg/synchronization/protocols/local"
	"github.com/mutagen-io/mutagen/pkg/synchronization/rsync"
	"golang.org/x/crypto/ssh/agent"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// ListConnections lists all connections this daemon is responsible for.
func (srv *Server) ListConnections(
	ctx context.Context,
	req *graftv1.ListConnectionsRequest,
) (*graftv1.ListConnectionsResponse, error) {
	connSnapshot := srv.connMgr.Connections()
	connections := make(map[string]*graftv1.ConnectionStatus, len(connSnapshot))

	var (
		selectedConn *Connection
		haveConn     bool
	)

	if req.GetPid() != 0 {
		currSess, err := srv.sessMgr.SessionByPID(req.GetPid())
		if err != nil {
			return nil, err
		}

		selectedConn, haveConn = srv.sessMgr.resolveSessionConnection(ctx, currSess)
	}

	for _, conn := range connSnapshot {
		state, reason := conn.State()
		d := conn.lockedDaemon()
		status := &graftv1.ConnectionStatus{
			State:           state.Proto(),
			SafeDestination: d.SafeDestination(),
		}

		if reason != "" {
			status.StateReason = &reason
		}

		if haveConn && selectedConn == conn {
			status.Current = true
		}

		for _, syncIntent := range conn.Synchronizations() {
			status.SyncStatuses = append(status.SyncStatuses, &graftv1.SyncStatus{
				FromLocal: syncIntent.FromLocal,
				ToRemote:  syncIntent.ToRemote,
			})
		}

		status.PortForwardStatuses = d.PortForwardStatuses()

		connections[conn.Name()] = status
	}

	// Build a lookup map: connectionName -> fromLocal -> *SyncStatus for matching with sync states.
	syncStatusLookup := make(map[string]map[string]*graftv1.SyncStatus)

	for connName, connStatus := range connections {
		for _, ss := range connStatus.GetSyncStatuses() {
			byLocal, ok := syncStatusLookup[connName]
			if !ok {
				byLocal = make(map[string]*graftv1.SyncStatus)
				syncStatusLookup[connName] = byLocal
			}

			byLocal[ss.GetFromLocal()] = ss
		}
	}

	_, syncStates, err := srv.synchronizationManager.List(ctx, &selection.Selection{All: true}, 0)
	if err != nil {
		slog.WarnContext(ctx, "failed to list sync states", "error", err)
	}

	for _, syncState := range syncStates {
		connName := syncState.GetSession().GetBeta().GetHost()

		localPath := syncState.GetSession().GetAlpha().GetPath()
		if byLocal, ok := syncStatusLookup[connName]; ok {
			if ss, ok := byLocal[localPath]; ok {
				populateSyncStatus(ss, syncState)
			}
		}
	}

	return &graftv1.ListConnectionsResponse{
		Connections: connections,
	}, nil
}

// populateSyncStatus fills in the detailed fields of a SyncStatus proto from a mutagen sync state.
// borrowed from mutagen (MIT code).
func populateSyncStatus(ss *graftv1.SyncStatus, state *synchronization.State) {
	ss.Paused = state.GetSession().GetPaused()
	ss.Status = state.GetStatus().Description()
	ss.LastError = state.GetLastError()
	// Populate structured conflicts.
	for _, c := range state.GetConflicts() {
		sc := &graftv1.SyncConflict{Path: formatPath(c.GetRoot())}
		for _, change := range c.GetAlphaChanges() {
			sc.LocalChanges = append(sc.LocalChanges, formatChange(change))
		}

		for _, change := range c.GetBetaChanges() {
			sc.RemoteChanges = append(sc.RemoteChanges, formatChange(change))
		}

		ss.Conflicts = append(ss.Conflicts, sc)
	}

	// Populate structured problems from both endpoints.
	appendProblems := func(problems []*core.Problem, source string) {
		for _, p := range problems {
			ss.Problems = append(ss.Problems, &graftv1.SyncProblem{
				Path:  formatPath(p.GetPath()),
				Error: p.GetError() + " (" + source + ")",
			})
		}
	}
	appendProblems(state.GetAlphaState().GetScanProblems(), "local scan")
	appendProblems(state.GetBetaState().GetScanProblems(), "remote scan")
	appendProblems(state.GetAlphaState().GetTransitionProblems(), "local transition")
	appendProblems(state.GetBetaState().GetTransitionProblems(), "remote transition")

	// Populate staging progress.
	var stagingProgress *rsync.ReceiverState

	switch state.GetStatus() { //nolint:exhaustive
	case synchronization.Status_StagingAlpha:
		stagingProgress = state.GetAlphaState().GetStagingProgress()
	case synchronization.Status_StagingBeta:
		stagingProgress = state.GetBetaState().GetStagingProgress()
	}

	if stagingProgress != nil {
		ss.StagingProgress = &graftv1.SyncStagingProgress{
			ReceivedFiles:       stagingProgress.GetReceivedFiles(),
			ExpectedFiles:       stagingProgress.GetExpectedFiles(),
			TotalReceivedSize:   stagingProgress.GetTotalReceivedSize(),
			CurrentPath:         stagingProgress.GetPath(),
			CurrentReceivedSize: stagingProgress.GetReceivedSize(),
			CurrentExpectedSize: stagingProgress.GetExpectedSize(),
		}
		if state.GetStatus() == synchronization.Status_StagingAlpha &&
			stagingProgress.GetExpectedFiles() == state.GetBetaState().GetFiles() {
			ss.StagingProgress.TotalExpectedSize = state.GetBetaState().GetTotalFileSize()
		} else if state.GetStatus() == synchronization.Status_StagingBeta &&
			stagingProgress.GetExpectedFiles() == state.GetAlphaState().GetFiles() {
			ss.StagingProgress.TotalExpectedSize = state.GetAlphaState().GetTotalFileSize()
		}
	}
}

// formatSyncStatusDescription builds a human-readable multi-line status description from the populated SyncStatus fields.
func formatSyncStatusDescription(ss *graftv1.SyncStatus) string {
	var b strings.Builder

	// Status line.
	if ss.GetPaused() {
		b.WriteString(color.YellowString("Status: Paused"))
	} else if sp := ss.GetStagingProgress(); sp != nil {
		stagingDir := "to remote"
		if strings.Contains(ss.GetStatus(), "alpha") {
			stagingDir = "to local"
		}

		b.WriteString(color.GreenString("Status: ") + "Staging files " + stagingDir)

		var pct float32
		if sp.GetTotalExpectedSize() > 0 {
			pct = float32(sp.GetTotalReceivedSize()) / float32(sp.GetTotalExpectedSize()) * 100
			b.WriteString(fmt.Sprintf("\n  Progress: %d/%d files (%s / %s) %.0f%%",
				sp.GetReceivedFiles(), sp.GetExpectedFiles(),
				humanize.Bytes(sp.GetTotalReceivedSize()), humanize.Bytes(sp.GetTotalExpectedSize()), pct))
		} else if sp.GetExpectedFiles() > 0 {
			pct = float32(sp.GetReceivedFiles()) / float32(sp.GetExpectedFiles()) * 100
			b.WriteString(fmt.Sprintf("\n  Progress: %d/%d files (%s) %.0f%%",
				sp.GetReceivedFiles(), sp.GetExpectedFiles(),
				humanize.Bytes(sp.GetTotalReceivedSize()), pct))
		}

		if sp.GetCurrentPath() != "" {
			b.WriteString(fmt.Sprintf("\n  Current file: %s (%s / %s)",
				sp.GetCurrentPath(),
				humanize.Bytes(sp.GetCurrentReceivedSize()), humanize.Bytes(sp.GetCurrentExpectedSize())))
		}
	} else {
		b.WriteString(color.GreenString("Status: ") + ss.GetStatus())
	}

	// Conflicts.
	if len(ss.GetConflicts()) > 0 {
		b.WriteString(color.YellowString("\n\nConflicts:"))

		for _, c := range ss.GetConflicts() {
			b.WriteString("\n  " + c.GetPath())

			for _, lc := range c.GetLocalChanges() {
				b.WriteString("\n    Local:  " + lc)
			}

			for _, rc := range c.GetRemoteChanges() {
				b.WriteString("\n    Remote: " + rc)
			}
		}
	}

	// Last error.
	if ss.GetLastError() != "" {
		b.WriteString(color.RedString("\n\nLast error: ") + ss.GetLastError())
	}

	// Problems.
	if len(ss.GetProblems()) > 0 {
		b.WriteString(color.RedString("\n\nProblems:"))

		for _, p := range ss.GetProblems() {
			b.WriteString(fmt.Sprintf("\n  %s: %s", p.GetPath(), p.GetError()))
		}
	}

	return b.String()
}

// formatEntry formats a mutagen filesystem entry for display.
func formatEntry(entry *core.Entry) string {
	if entry == nil {
		return "<non-existent>"
	}

	switch entry.GetKind() { //nolint:exhaustive
	case core.EntryKind_Directory:
		return "Directory"
	case core.EntryKind_File:
		if entry.GetExecutable() {
			return fmt.Sprintf("Executable File (%x)", entry.GetDigest())
		}

		return fmt.Sprintf("File (%x)", entry.GetDigest())
	case core.EntryKind_SymbolicLink:
		return fmt.Sprintf("Symbolic Link (%s)", entry.GetTarget())
	case core.EntryKind_Untracked:
		return "Untracked content"
	case core.EntryKind_Problematic:
		return fmt.Sprintf("Problematic content (%s)", entry.GetProblem())
	default:
		return "<unknown>"
	}
}

// formatChange formats a mutagen change (old -> new entry) for display.
func formatChange(change *core.Change) string {
	return formatEntry(change.GetOld()) + " -> " + formatEntry(change.GetNew())
}

// formatPath returns the display name for a sync path, using <root> for empty paths.
func formatPath(p string) string {
	if p == "" {
		return "<root>"
	}

	return p
}

// InitializeSSHConnection initializes an SSH based connection.
func (srv *Server) InitializeSSHConnection(
	ctx context.Context,
	req *graftv1.InitializeSSHConnectionRequest,
) (*graftv1.InitializeSSHConnectionResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	// TODO(erd): Add validation for destination, user, and name parameters.
	dest := req.GetDestination()
	user := req.GetUserName()
	name := req.GetName()

	destURL, err := ParseDestination(dest)
	if err != nil {
		return nil, err
	}

	destURL.Scheme = sshSchemeName
	if user != "" && destURL.User == nil {
		destURL.User = url.User(user)
	}

	slog.InfoContext(ctx, "InitializeSSHConnection",
		"request", req,
		"dest", destURL.String(),
	)
	localRoot := req.GetLocalRoot()
	remoteRoot := req.GetRemoteRoot()

	background := req.GetBackground()

	conn, err := srv.connMgr.Initialize(ctx, name, destURL, localRoot, remoteRoot, srv.identity, background)
	if err != nil {
		return nil, err
	}

	// TODO(erd): Make the code for this the same path as docker to have no bugs
	srv.rootConfig.Connections = append(srv.rootConfig.Connections, ConnectionConfig{
		Name:        conn.Name(),
		Destination: conn.daemon.Destination(),
		LocalRoot:   localRoot,
		RemoteRoot:  remoteRoot,
		Background:  background,
	})
	srv.persistConfig()

	return &graftv1.InitializeSSHConnectionResponse{Name: conn.Name()}, nil
}

// InitializeContainerConnection initializations a docker based connection.
func (srv *Server) InitializeContainerConnection(
	ctx context.Context,
	req *graftv1.InitializeContainerConnectionRequest,
) (*graftv1.InitializeContainerConnectionResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	// TODO(erd): better name... fix it up too for illegal term chars
	name := ResolveConnectionName(req.GetName(), req.GetOperatingSystem())

	// Host is the container id, to be filled in later
	var destURL url.URL

	destURL.Scheme = dockerSchemeName
	query := destURL.Query()
	query.Add("name", name)
	query.Add("imageTag", req.GetImageTag())
	destURL.RawQuery = query.Encode()

	slog.InfoContext(ctx, "InitializeContainerConnection",
		"request", req,
		"dest", destURL.String(),
	)

	localRoot := req.GetLocalRoot()
	remoteRoot := req.GetRemoteRoot()

	background := req.GetBackground()

	conn, err := srv.connMgr.Initialize(ctx, name, &destURL, localRoot, remoteRoot, srv.identity, background)
	if err != nil {
		return nil, err
	}

	srv.rootConfig.Connections = append(srv.rootConfig.Connections, ConnectionConfig{
		Name:        conn.Name(),
		Destination: conn.daemon.Destination(),
		LocalRoot:   localRoot,
		RemoteRoot:  remoteRoot,
		Background:  background,
	})
	srv.persistConfig()

	return &graftv1.InitializeContainerConnectionResponse{Name: conn.Name()}, nil
}

// RemoveConnection tears down the specified connection.
func (srv *Server) RemoveConnection(
	ctx context.Context,
	req *graftv1.RemoveConnectionRequest,
) (*graftv1.RemoveConnectionResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	slog.InfoContext(ctx, "RemoveConnection", "request", req, "name", req.GetName())

	err := srv.connMgr.Remove(ctx, req.GetName())
	if err != nil {
		return nil, err
	}

	srv.rootConfig.Connections = slices.DeleteFunc(srv.rootConfig.Connections, func(cc ConnectionConfig) bool {
		return cc.Name == req.GetName()
	})
	srv.persistConfig()

	return &graftv1.RemoveConnectionResponse{}, nil
}

// DiscoverCommands streams all commands a daemon thinks it can allow a client to forward.
// TODO(erd): the user and some kind of session probably matters, but avoid for now.
func (srv *Server) DiscoverCommands(req *graftv1.DiscoverCommandsRequest, server graftv1.GraftService_DiscoverCommandsServer) error {
	var knownCommands []string

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// TODO(erd): Filter discovered commands using the allow list constructed above.
	allowList := make(map[string]bool, len(req.GetAllowList()))
	for _, allowed := range req.GetAllowList() {
		allowList[allowed] = true
	}

	for {
		// Periodic refresh for known directories via env providers.
		if srv.envProviders != nil && len(req.GetDirectories()) > 0 {
			srv.envProviders.Refresh(server.Context(), req.GetDirectories())
		}

		var extraPathDirs []string
		if srv.envProviders != nil {
			extraPathDirs = srv.envProviders.ExtraPATHDirs()
		}

		commands := collectCommandsFromPATH(extraPathDirs...)
		if !slices.Equal(knownCommands, commands) {
			knownCommands = commands

			err := server.Send(&graftv1.DiscoverCommandsResponse{
				Commands: commands,
			})
			if err != nil {
				slog.DebugContext(server.Context(), "error sending response", "error", err)

				return errors.Wrap(err)
			}
		}

		select {
		case <-ticker.C:
		case <-server.Context().Done():
			return nil
		}
	}
}

// UpdateConnectionRoots updates the local and/or remote root directories for a connection.
func (srv *Server) UpdateConnectionRoots(
	_ context.Context,
	req *graftv1.UpdateConnectionRootsRequest,
) (*graftv1.UpdateConnectionRootsResponse, error) {
	srv.serverMu.Lock()
	defer srv.serverMu.Unlock()

	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	if err := conn.SetRoots(req.GetLocalRoot(), req.GetRemoteRoot()); err != nil {
		return nil, errors.Wrap(err)
	}

	srv.connMgr.RefreshConnectionRootsFile()

	for i := range srv.rootConfig.Connections {
		if srv.rootConfig.Connections[i].Name == conn.Name() {
			if req.GetLocalRoot() != "" {
				srv.rootConfig.Connections[i].LocalRoot = req.GetLocalRoot()
			}

			if req.GetRemoteRoot() != "" {
				srv.rootConfig.Connections[i].RemoteRoot = req.GetRemoteRoot()
			}

			break
		}
	}

	srv.persistConfig()

	return &graftv1.UpdateConnectionRootsResponse{}, nil
}

// UpdateConnectionForwardCommands adds the specified commands to be forwarded for a connection.
//
// TODO(erd): should this be replace or insert?
func (srv *Server) UpdateConnectionForwardCommands(
	_ context.Context,
	req *graftv1.UpdateConnectionForwardCommandsRequest,
) (*graftv1.UpdateConnectionForwardCommandsResponse, error) {
	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	fwdList := make([]ForwardCommandIntent, 0, len(req.GetCommands()))
	for _, cmd := range req.GetCommands() {
		fwdList = append(fwdList, ForwardCommandIntent{
			Name:   cmd,
			Prefix: req.GetPrefixCommands(),
		})
	}

	if err := srv.UpdateForwardCommands(fwdList, conn.Name()); err != nil {
		return nil, err
	}

	return &graftv1.UpdateConnectionForwardCommandsResponse{}, nil
}

// RemoveConnectionForwardCommands removes the specified commands from being forwarded for a connection.
func (srv *Server) RemoveConnectionForwardCommands(
	_ context.Context,
	req *graftv1.RemoveConnectionForwardCommandsRequest,
) (*graftv1.RemoveConnectionForwardCommandsResponse, error) {
	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	if err := srv.RemoveForwardCommands(req.GetCommands(), conn.Name()); err != nil {
		return nil, err
	}

	return &graftv1.RemoveConnectionForwardCommandsResponse{}, nil
}

// SyncFilesToConnection sets up a bidirectional synchronization between the local and remote daemons.
func (srv *Server) SyncFilesToConnection(
	ctx context.Context,
	req *graftv1.SyncFilesToConnectionRequest,
) (*graftv1.SyncFilesToConnectionResponse, error) {
	conn, err := srv.connMgr.Connection(req.GetToConnectionName())
	if err != nil {
		return nil, err
	}

	toRemote := req.GetDestDir()
	if toRemote == "" {
		toRemote = defaultSyncRemotePath(conn.HomeDir(), srv.identity, req.GetSourceDir())
	}

	syncIntent := SynchronizationIntent{
		FromLocal: req.GetSourceDir(),
		ToRemote:  toRemote,
	}
	if err := srv.connMgr.EstablishSynchronization(
		ctx,
		req.GetToConnectionName(),
		syncIntent,
		srv.synchronizationManager,
		srv.syncProtoNum,
	); err != nil {
		return nil, err
	}

	if err := srv.UpdateSynchronizations(req.GetToConnectionName()); err != nil {
		return nil, err
	}

	return &graftv1.SyncFilesToConnectionResponse{}, nil
}

// defaultSyncRemotePath computes a unique default remote sync directory when the user
// hasn't specified one. The path includes the local daemon identity (per-machine isolation)
// and a short hash of the source directory (per-project isolation) to prevent collisions
// when multiple machines or projects sync to the same remote host.
func defaultSyncRemotePath(homeDir, identity, sourceDir string) string {
	hash := sha256.Sum256([]byte(sourceDir))
	shortHash := hex.EncodeToString(hash[:])[:6]

	return filepath.Join(homeDir, ".graft", "sync", identity, filepath.Base(sourceDir)+"-"+shortHash)
}

// SyncFilesToConnectionProtocol is used for the underlying mutagen sync protocol that bypasses
// the mutagen daemon installation process. We do this bypass because:
// - we were curious how easy it was to do this given mutagen/graft are both written in go
// - it affords us one installation, making updates and maintenance a lot easier
// - allows us to move away from mutagen if needed
//
// The protocol is binary/opaque to us. It's a protobuf based protocol but not gRPC. We could actually
// implement the idea of local/remote endpoints but it's a lot easier to just pass it through for now.
func (srv *Server) SyncFilesToConnectionProtocol(server graftv1.GraftService_SyncFilesToConnectionProtocolServer) error {
	stream := newMutagenSyncStreamServerWrapper(server)

	if err := remote.ServeEndpoint(logging.NewLoggerOnSlogger(slog.Default()), stream); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

func (srv *Server) DumpLogs(ctx context.Context, req *graftv1.DumpLogsRequest) (*graftv1.DumpLogsResponse, error) {
	conn, err := srv.connMgr.Connection(req.GetConnectionName())
	if err != nil {
		return nil, err
	}

	stdout, stderr, err := conn.DumpLogs(ctx)
	if err != nil {
		return nil, errors.WrapPrefix(err, "error dumping logs")
	}

	return &graftv1.DumpLogsResponse{
		Stdout: stdout,
		Stderr: stderr,
	}, nil
}

func (srv *Server) ForwardSSHAgent(server graftv1.GraftService_ForwardSSHAgentServer) error {
	if srv.role != ServerRoleRemote {
		// Peek at the first message to get the connection name, then forward it.
		firstMsg, err := server.Recv()
		if err != nil {
			return errors.Wrap(err)
		}

		connName := firstMsg.GetConnectionName()
		if connName == "" {
			return errors.New("ForwardSSHAgent: connection_name is required")
		}

		// TODO(erd): Consider integrating SSH agent forwarding into connection establishment procedures.
		conn, err := srv.connMgr.Connection(connName)
		if err != nil {
			return err
		}

		return errors.Wrap(conn.ForwardSSHAgent(server.Context()))
	}

	agentOverGRPCClient := agent.NewClient(newForwardSSHAgentStreamServerWrapper(server))

	sockFile, err := os.CreateTemp("", "graft-ssh-agent-*.sock")
	if err != nil {
		return errors.Wrap(err)
	}

	sockFile.Close()
	os.Remove(sockFile.Name())

	slog.DebugContext(server.Context(), "local ssh agent uds sock", "path", sockFile.Name())

	listener, err := net.Listen("unix", sockFile.Name()) //nolint:noctx
	if err != nil {
		// TODO(erd): Listen failure may leave restore goroutine blocked indefinitely.
		return errors.Wrap(err)
	}

	// TODO(erd): only allow one of these...
	srv.serverMu.Lock()
	srv.sshAuthSockPath = sockFile.Name()
	srv.serverMu.Unlock()

	defer func() {
		srv.serverMu.Lock()
		srv.sshAuthSockPath = ""
		srv.serverMu.Unlock()
	}()

	for {
		nextConn, err := listener.Accept()
		if err != nil {
			return errors.Wrap(err)
		}

		go func() {
			defer func() {
				slog.Debug("done serving ssh agent connection")
			}()

			slog.Debug("serving ssh agent connection")

			if err := agent.ServeAgent(agentOverGRPCClient, nextConn); err != nil {
				slog.Error("error serving ssh agent", "error", err)
			}
		}()
	}
}

package graft

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"
	"golang.org/x/term"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// Shutdown tells the daemon to shutdown.
func (client *LocalClient) Shutdown(ctx context.Context) error {
	if _, err := client.GraftServiceClient.Shutdown(ctx, &graftv1.ShutdownRequest{}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// Restart tells the daemon to restart.
func (client *LocalClient) Restart(ctx context.Context) error {
	if _, err := client.GraftServiceClient.Restart(ctx, &graftv1.RestartRequest{}); err != nil {
		return client.handleError(err)
	}

	return nil
}

func (client *LocalClient) sessionPID() (uint64, bool) {
	sessStr, ok := os.LookupEnv("GRAFT_SESSION")
	if !ok {
		return 0, false
	}

	sessPID, err := strconv.ParseUint(sessStr, 10, 64)
	if err != nil {
		fmt.Fprintf(client.errWriter, "malformed GRAFT_SESSION='%s'\n", sessStr)
	}

	return sessPID, true
}

// GetStatus returns the formatted status string for each connection.
func (client *LocalClient) GetStatus(ctx context.Context) (string, error) {
	req := &graftv1.ListConnectionsRequest{}

	sessPID, sessPIDOk := client.sessionPID()
	if sessPIDOk {
		req.Pid = sessPID
	}

	resp, err := client.ListConnections(ctx, req)
	if err != nil {
		return "", client.handleError(err)
	}

	var buf strings.Builder

	if len(resp.GetConnections()) == 0 {
		buf.WriteString("No connections 😎\n")

		return buf.String(), nil
	}

	buf.WriteString("Connections:\n")

	connNames := make([]string, 0, len(resp.GetConnections()))
	for name := range resp.GetConnections() {
		connNames = append(connNames, name)
	}

	sortConnectionNames(connNames, resp.GetConnections())

	for _, name := range connNames {
		status := resp.GetConnections()[name]
		fmt.Fprintf(&buf, "%s (%s)", name, status.GetSafeDestination())

		if reason := status.GetStateReason(); reason != "" {
			fmt.Fprintf(&buf, " (%s: %s)", connectionStateDisplayString(status.GetState()), reason)
		} else {
			fmt.Fprintf(&buf, " (%s)", connectionStateDisplayString(status.GetState()))
		}

		if status.GetCurrent() {
			buf.WriteString(" [current]")
		}

		hasSyncs := len(status.GetSyncStatuses()) != 0
		hasPorts := len(status.GetPortForwardStatuses()) != 0

		if hasSyncs || hasPorts {
			buf.WriteString(":")
		}

		for _, syncStatus := range status.GetSyncStatuses() {
			fmt.Fprintf(&buf, "\n  Synchronizing %s -> %s", syncStatus.GetFromLocal(), syncStatus.GetToRemote())

			indented := strings.ReplaceAll(formatSyncStatusDescription(syncStatus), "\n", "\n    ")
			fmt.Fprintf(&buf, "\n    %s", indented)
		}

		for _, pf := range status.GetPortForwardStatuses() {
			if pf.GetConflict() {
				fmt.Fprintf(&buf, "\n  %s %s %d: %s",
					color.RedString("CONFLICT"),
					pf.GetProtocol(),
					pf.GetRemotePort(),
					pf.GetConflictReason())
			} else {
				fmt.Fprintf(&buf, "\n  Forwarding %s localhost:%d -> remote:%d",
					pf.GetProtocol(),
					pf.GetLocalPort(),
					pf.GetRemotePort())
			}
		}

		buf.WriteString("\n")
	}

	return buf.String(), nil
}

// PrintStatus prints the status of each connection.
func (client *LocalClient) PrintStatus(ctx context.Context) error {
	s, err := client.GetStatus(ctx)
	if err != nil {
		return err
	}

	fmt.Fprint(client.errWriter, s)

	return nil
}

// GetStatusJSON returns the status of each connection as a JSON string.
func (client *LocalClient) GetStatusJSON(ctx context.Context) (string, error) {
	req := &graftv1.ListConnectionsRequest{}

	sessPID, sessPIDOk := client.sessionPID()
	if sessPIDOk {
		req.Pid = sessPID
	}

	resp, err := client.ListConnections(ctx, req)
	if err != nil {
		return "", client.handleError(err)
	}

	marshaler := protojson.MarshalOptions{
		UseProtoNames: true,
	}

	data, err := marshaler.Marshal(resp)
	if err != nil {
		return "", errors.WrapPrefix(err, "marshaling status to JSON")
	}

	return string(data) + "\n", nil
}

// PrintStatusJSON prints the status of each connection as JSON to stdout.
func (client *LocalClient) PrintStatusJSON(ctx context.Context) error {
	s, err := client.GetStatusJSON(ctx)
	if err != nil {
		return err
	}

	fmt.Fprint(client.outWriter, s)

	return nil
}

// PrintDaemonLogs prints recent daemon logs with colored rendering.
func (client *LocalClient) PrintDaemonLogs(ctx context.Context) error {
	resp, err := client.Status(ctx, &graftv1.StatusRequest{})
	if err != nil {
		return client.handleError(err)
	}

	logs := resp.GetRecentLogs()
	if len(logs) == 0 {
		fmt.Fprintln(client.errWriter, "No recent logs")

		return nil
	}

	for _, logLine := range logs {
		RenderJSONLogLine(client.errWriter, logLine)
	}

	return nil
}

// PrintDaemonStatus prints daemon health, version, and recent logs with colored rendering.
func (client *LocalClient) PrintDaemonStatus(ctx context.Context, connectionName string) error {
	req := &graftv1.StatusRequest{}
	if connectionName != "" {
		req.ConnectionName = new(connectionName)
	}

	resp, err := client.Status(ctx, req)
	if err != nil {
		return client.handleError(err)
	}

	w := client.errWriter

	healthy := color.GreenString("healthy")
	if !resp.GetHealthy() {
		healthy = color.RedString("unhealthy")
	}

	fmt.Fprintf(w, "Daemon:  %s\n", color.GreenString("running"))
	fmt.Fprintf(w, "Version: %s\n", versionString(resp.GetVersionInfo()))
	fmt.Fprintf(w, "Health:  %s\n", healthy)
	fmt.Fprintf(w, "Uptime:  %s\n", resp.GetUptime().AsDuration().String())

	if logs := resp.GetRecentLogs(); len(logs) > 0 {
		fmt.Fprintln(w, "\nRecent logs:")

		for _, logLine := range logs {
			fmt.Fprint(w, "  ")
			RenderJSONLogLine(w, logLine)
		}
	}

	return nil
}

// Watch polls getFn every second, overwriting any printed output until the context is cancelled.
func (client *LocalClient) Watch(ctx context.Context, getFn func(context.Context) (string, error)) error {
	fw := newFlushingWriter(client.errWriter)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		s, err := getFn(ctx)
		if err != nil {
			if errors.Is(context.Cause(ctx), context.Canceled) {
				return nil
			}

			return err
		}

		fw.Flush()
		fw.Write(s)

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// RemoveConnection removes and potenitally tears down the daemon bound to the given connection name.
func (client *LocalClient) RemoveConnection(ctx context.Context, name string) error {
	if _, err := client.GraftServiceClient.RemoveConnection(ctx, &graftv1.RemoveConnectionRequest{
		Name: name,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// PrintShimmedCommands prints a lis tof this session's commands being shimmed and where to.
func (client *LocalClient) PrintShimmedCommands(ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		return client.handleError(err)
	}

	resp, err := client.SessionShimmedCommands(ctx, &graftv1.SessionShimmedCommandsRequest{
		Pid: uint64(os.Getppid()), //nolint:gosec // overflow okay
		Cwd: cwd,
	})
	if err != nil {
		return client.handleError(err)
	}

	for dest, cmds := range resp.GetDestinationCommands() {
		fmt.Fprintf(client.errWriter, "%s\n", dest)

		for _, cmd := range cmds.GetCommands() {
			fmt.Fprintf(client.errWriter, "\t%s (%s)\n", cmd.GetLocal(), cmd.GetRemote())
		}
	}

	return nil
}

// Ping records RTT times to the local daemon this client is connected to.
func (client *LocalClient) Ping(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		beforePing := time.Now()

		pingResp, err := client.GraftServiceClient.Ping(ctx, &graftv1.PingRequest{
			LocalTimeUnixNanos: beforePing.UnixNano(),
		})
		if err != nil {
			if errors.Is(context.Cause(ctx), context.Canceled) {
				return nil
			}

			return client.handleError(err)
		}

		afterPing := time.Now()
		fmt.Fprintf(client.errWriter,
			"%s=%s rtt=%s \n",
			color.GreenString("remote->local"),
			afterPing.Sub(time.Unix(0, pingResp.GetLocalTimeUnixNanos())).String(),
			afterPing.Sub(beforePing).String())

		select {
		case <-ticker.C:
			if context.Cause(ctx) != nil {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// ConnectParams bundles all parameters for establishing a connection.
type ConnectParams struct {
	Name       string
	LocalRoot  string
	RemoteRoot string

	// SSH-specific fields.
	Destination string
	Username    string

	// Docker-specific fields.
	ImageTag string
	OSName   string

	// Post-init options.
	ForwardCommands []string
	ForwardPrefix   bool
	PortForwards    []string // explicit port forward specs (e.g. "8080", "3000:8080/tcp")
	WithSync        bool

	// SyncSource/SyncDest override LocalRoot/RemoteRoot for sync when set.
	SyncSource string
	SyncDest   string

	// Background excludes this connection from CWD-based auto-selection.
	Background bool
}

// InitializeRemoteConnection sets up an SSH based connection.
func (client *LocalClient) InitializeRemoteConnection(ctx context.Context, params ConnectParams) error {
	if params.LocalRoot != "" {
		info, err := os.Stat(params.LocalRoot)
		if err != nil {
			return errors.Wrap(err)
		}

		if !info.IsDir() {
			return errors.Errorf("'%s' is not a directory", params.LocalRoot)
		}
	}

	fmt.Fprintf(client.errWriter, "initializing %s\n", params.Destination)

	resp, err := client.InitializeSSHConnection(ctx, &graftv1.InitializeSSHConnectionRequest{
		Name:        params.Name,
		Destination: params.Destination,
		UserName:    params.Username,
		Pid:         uint64(os.Getppid()), //nolint:gosec // overflow okay
		LocalRoot:   params.LocalRoot,
		RemoteRoot:  params.RemoteRoot,
		Background:  params.Background,
	})
	if err != nil {
		return client.handleError(err)
	}

	return client.postInitConnection(ctx, resp.GetName(), params)
}

// InitializeDockerConnection sets up a docker based connection by creating a new container.
func (client *LocalClient) InitializeDockerConnection(ctx context.Context, params ConnectParams) error {
	initName := ResolveConnectionName(params.Name, params.OSName)

	fmt.Fprintf(client.errWriter, "initializing %s\n", initName)

	resp, err := client.InitializeContainerConnection(ctx, &graftv1.InitializeContainerConnectionRequest{
		Name:            initName,
		ImageTag:        params.ImageTag,
		OperatingSystem: params.OSName,
		LocalRoot:       params.LocalRoot,
		RemoteRoot:      params.RemoteRoot,
		Background:      params.Background,
	})
	if err != nil {
		return client.handleError(err)
	}

	return client.postInitConnection(ctx, resp.GetName(), params)
}

func (client *LocalClient) postInitConnection(
	ctx context.Context,
	resolvedConnectionName string,
	params ConnectParams,
) error {
	if len(params.ForwardCommands) > 0 {
		if fwdErr := client.ForwardCommands(ctx, params.ForwardCommands, resolvedConnectionName,
			params.ForwardPrefix,
		); fwdErr != nil {
			return fwdErr
		}
	}

	if len(params.PortForwards) > 0 {
		if fwdErr := client.AddPortForwards(ctx, params.PortForwards, resolvedConnectionName); fwdErr != nil {
			return fwdErr
		}
	}

	if params.WithSync {
		syncSource := params.LocalRoot
		syncDest := params.RemoteRoot

		if params.SyncSource != "" {
			syncSource = params.SyncSource
		}

		if params.SyncDest != "" {
			syncDest = params.SyncDest
		}

		if syncErr := client.Sync(ctx, syncSource, syncDest, resolvedConnectionName); syncErr != nil {
			return syncErr
		}
	}

	client.printConnectSummary(resolvedConnectionName, params.LocalRoot, params.RemoteRoot, params.WithSync)

	return nil
}

func (client *LocalClient) printConnectSummary(name, localRoot, remoteRoot string, synced bool) {
	fmt.Fprintf(client.errWriter, "connected to %s\n", name)

	if localRoot != "" {
		fmt.Fprintf(client.errWriter, "  local root:   %s\n", localRoot)

		if remoteRoot != "" {
			fmt.Fprintf(client.errWriter, "  remote root:  %s\n", remoteRoot)
		}

		if synced {
			fmt.Fprintf(client.errWriter, "  sync:         enabled\n")
		}
	} else {
		fmt.Fprintf(client.errWriter, "\n")
		fmt.Fprintf(client.errWriter, "  Run commands:    graft run --to %s <command>\n", name)
		fmt.Fprintf(client.errWriter, "  Open a shell:    graft shell --to %s\n", name)
		fmt.Fprintf(client.errWriter, "  Set local root:  graft connection set-root %s <local_dir> [remote_dir]\n", name)
	}
}

// Sync sets up bidi file sync between the source directory and a connection.
func (client *LocalClient) Sync(ctx context.Context, sourceDir, destDir, toConnName string) error {
	if sourceDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return client.handleError(err)
		}

		sourceDir = cwd
	}

	if _, err := client.SyncFilesToConnection(ctx, &graftv1.SyncFilesToConnectionRequest{
		SourceDir:        sourceDir,
		DestDir:          destDir,
		ToConnectionName: toConnName,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// SetConnectionRoots updates the local and/or remote root directories for a connection.
func (client *LocalClient) SetConnectionRoots(ctx context.Context, connName, localRoot, remoteRoot string) error {
	if _, err := client.UpdateConnectionRoots(ctx, &graftv1.UpdateConnectionRootsRequest{
		ConnectionName: connName,
		LocalRoot:      localRoot,
		RemoteRoot:     remoteRoot,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// ReportCWD notifies the daemon of the most recent current working directory of this session (indentified by PID).
// TODO(erd): this name makes it seem like you'd get output. maybe updatecwd.
func (client *LocalClient) ReportCWD(ctx context.Context, pid uint64, cwd string) error {
	if _, err := client.SessionReportCWD(ctx, &graftv1.SessionReportCWDRequest{
		Pid: pid,
		Cwd: cwd,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// RunCommandOptions are used to run any kind of command:
// - Shell (inferred/explicit)
// - Command (inferred/explicit).
type RunCommandOptions struct {
	CallerPID      uint64 // used to identify current session
	ConnectionName string // used to explicitly set desired connection name
	CWD            string // used to implicitly set desired connection name

	Command   string
	Arguments []string

	MakeShell bool // spawn a shell; ignore command

	ExactCommand bool // Is this exact command to send to the inferred connection without prefix/forward matching?
	WithSudo     bool // Run as sudo (root)
}

// RunCommand runs an explicitly provided command (e.g. graft cmd blah).
func (client *LocalClient) RunCommand(
	ctx context.Context,
	command string,
	arguments []string,
	connectionName string,
) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 0, client.handleError(err)
	}

	return client.runCommand(ctx, RunCommandOptions{
		ConnectionName: connectionName,
		CallerPID:      uint64(os.Getppid()), //nolint:gosec // overflow okay
		CWD:            cwd,
		Command:        command,
		Arguments:      arguments,
		ExactCommand:   true,
	})
}

// RunShimmedCommand runs a command caught by the shimming shell interface.
func (client *LocalClient) RunShimmedCommand(
	ctx context.Context,
	command string,
	arguments []string,
	pid uint64,
	cwd string,
	withSudo bool,
) (int, error) {
	return client.runCommand(ctx, RunCommandOptions{
		CallerPID: pid,
		CWD:       cwd,
		Command:   command,
		Arguments: arguments,
		WithSudo:  withSudo,
	})
}

// RemoteShell spawns an interactive shell on a connection.
func (client *LocalClient) RemoteShell(
	ctx context.Context,
	connectionName string,
) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return 0, client.handleError(err)
	}

	callerPID := uint64(os.Getppid()) //nolint:gosec // overflow okay
	if sessPID, ok := client.sessionPID(); ok {
		callerPID = sessPID
	}

	return client.runCommand(ctx, RunCommandOptions{
		CallerPID:      callerPID,
		CWD:            cwd,
		MakeShell:      true,
		ConnectionName: connectionName,
	})
}

// RunCommandGRPCClientHandler processes a running command on the client side of a RunCommand. That means
// the [RunningCommand] is coming from a remote daemon.
type RunCommandGRPCClientHandler struct {
	stdin         io.Writer
	stdout        io.Reader
	stderr        io.Reader
	runningCmd    RunningCommand
	outputWriters sync.WaitGroup
}

func (h *RunCommandGRPCClientHandler) Handle(ctx context.Context) (int, error) {
	sigWinChChan := make(chan os.Signal, 1)
	defer close(sigWinChChan)

	signal.Notify(sigWinChChan, syscall.SIGWINCH)

	go func() {
		for range sigWinChChan {
			width, height, err := term.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				return
			}

			if err := h.runningCmd.NotifyWindowChange(height, width); err != nil {
				return
			}
		}
	}()

	sigWinChChan <- syscall.SIGWINCH

	go func() {
		defer h.runningCmd.Stdin().Close()

		if _, err := io.Copy(h.runningCmd.Stdin(), os.Stdin); err != nil {
			slog.ErrorContext(ctx, "error copying stdin", "error", err)
		}
	}()

	defer h.outputWriters.Wait()

	h.outputWriters.Go(func() {
		if _, err := io.Copy(os.Stdout, h.runningCmd.Stdout()); err != nil {
			slog.ErrorContext(ctx, "error copying stdout", "error", err)
		}
	})
	h.outputWriters.Go(func() {
		if _, err := io.Copy(os.Stderr, h.runningCmd.Stderr()); err != nil {
			slog.ErrorContext(ctx, "error copying stderr", "error", err)
		}
	})

	waitStatus, waitErr := h.runningCmd.Wait()

	// unblock anything waiting on stdin
	h.runningCmd.Stdin().Close()

	// wait to process stdout/err
	h.outputWriters.Wait()

	if waitErr != nil {
		return -1, errors.Wrap(waitErr)
	}

	return waitStatus, nil
}

func (client *LocalClient) runCommand(
	ctx context.Context,
	opts RunCommandOptions,
) (int, error) {
	runClient, err := client.GraftServiceClient.RunCommand(ctx)
	if err != nil {
		return 0, client.handleError(err)
	}

	var envToPass []string
	// Security (see main.go): Export env vars directly input from the command line, not the environment
	if inlineVars := os.Getenv("__INLINE_VARS"); inlineVars != "" {
		inlineVars := strings.Split(inlineVars, ",")
		envToPass = slices.DeleteFunc(os.Environ(), func(keyVal string) bool {
			split := strings.SplitN(keyVal, "=", 2)
			if len(split) != 2 {
				return true
			}

			return !slices.Contains(inlineVars, split[0])
		})
	}

	stdinIsTerminal := term.IsTerminal(int(os.Stdin.Fd()))
	stdoutIsTerminal := true

	if f, ok := client.outWriter.(*os.File); ok {
		stdoutIsTerminal = term.IsTerminal(int(f.Fd()))
	}

	// Only allocate a pty when both stdin and stdout are terminals.
	// When output is piped, a pty causes spurious echo/padding artifacts.
	allocatePty := stdinIsTerminal && stdoutIsTerminal
	startReq := &graftv1.RunCommandRequest_Start{
		Start: &graftv1.StartCommand{
			Pid:            opts.CallerPID,
			ConnectionName: opts.ConnectionName,
			Cwd:            opts.CWD,

			Command:      opts.Command,
			Arguments:    opts.Arguments,
			Sudo:         opts.WithSudo,
			ExactCommand: opts.ExactCommand,
			ExtraEnv:     envToPass,

			Shell: opts.MakeShell,

			AllocatePty:    allocatePty,
			RedirectStdout: true,
			RedirectStderr: true,
		},
	}
	runCmdReq := &graftv1.RunCommandRequest{
		Data: startReq,
	}

	if f, ok := client.outWriter.(*os.File); ok {
		startReq.Start.RedirectStdout = !term.IsTerminal(int(f.Fd()))
	}

	if f, ok := client.errWriter.(*os.File); ok {
		startReq.Start.RedirectStderr = !term.IsTerminal(int(f.Fd()))
	}

	sendErr := runClient.Send(runCmdReq)
	if sendErr != nil {
		return 0, errors.Wrap(sendErr)
	}

	// Wait for the server to confirm the command is running before entering raw mode.
	// While waiting, the terminal stays in cooked mode so Ctrl-C generates SIGINT normally.
	resp, err := runClient.Recv()
	if err != nil {
		return 0, errors.Wrap(err)
	}

	if _, ok := resp.GetData().(*graftv1.RunCommandResponse_Started); !ok {
		return 0, errors.New("expected CommandStarted response from server")
	}

	runningCmd := NewRemoteRunningCommand(runClient)

	// TODO(erd): Implement signal forwarding to remote commands.
	// term shit here

	if stdinFd := int(os.Stdin.Fd()); allocatePty {
		oldState, err := term.MakeRaw(stdinFd)
		if err != nil {
			panic(client.handleError(err))
		}

		defer func() {
			err := term.Restore(int(os.Stdin.Fd()), oldState)
			if err != nil {
				slog.ErrorContext(ctx, "error restoring terminal state", "error", err)
			}
		}()
	}

	handler := RunCommandGRPCClientHandler{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		runningCmd: runningCmd,
	}

	return handler.Handle(ctx)
}

// ForwardCommands updates the commands to forward for the connection associated with the connection name.
func (client *LocalClient) ForwardCommands(ctx context.Context, commands []string, connectionName string, withPrefix bool) error {
	commandsToFwd := checkForwardCommands(commands, withPrefix, connectionName)
	if len(commandsToFwd) == 0 {
		return nil
	}

	if _, err := client.UpdateConnectionForwardCommands(ctx, &graftv1.UpdateConnectionForwardCommandsRequest{
		ConnectionName: connectionName,
		Commands:       commandsToFwd,
		// TODO(erd): why do we forward these and then also have the prefix bool still? bug?
		PrefixCommands: withPrefix,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// RemoveForwardCommands removes the specified commands from being forwarded for a connection.
func (client *LocalClient) RemoveForwardCommands(ctx context.Context, commands []string, connectionName string) error {
	if _, err := client.RemoveConnectionForwardCommands(ctx, &graftv1.RemoveConnectionForwardCommandsRequest{
		ConnectionName: connectionName,
		Commands:       commands,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// AddPortForwards adds explicit port forwards to a connection.
func (client *LocalClient) AddPortForwards(ctx context.Context, portSpecs []string, connectionName string) error {
	specs, err := ParsePortSpecsToProto(portSpecs)
	if err != nil {
		return err
	}

	if _, err := client.GraftServiceClient.AddPortForwards(ctx, &graftv1.AddPortForwardsRequest{
		ConnectionName: connectionName,
		Ports:          specs,
	}); err != nil {
		return client.handleError(err)
	}

	return nil
}

// RemovePortForwards removes explicit port forwards from a connection.
// Returns the list of ports that were only auto-detected (not explicitly forwarded).
func (client *LocalClient) RemovePortForwards(ctx context.Context, portSpecs []string, connectionName string) ([]PortForwardSpec, error) {
	specs, err := ParsePortSpecsToProto(portSpecs)
	if err != nil {
		return nil, err
	}

	resp, err := client.GraftServiceClient.RemovePortForwards(ctx, &graftv1.RemovePortForwardsRequest{
		ConnectionName: connectionName,
		Ports:          specs,
	})
	if err != nil {
		return nil, client.handleError(err)
	}

	autoDetected := make([]PortForwardSpec, 0, len(resp.GetAutoDetectedPorts()))
	for _, p := range resp.GetAutoDetectedPorts() {
		autoDetected = append(autoDetected, PortForwardSpecFromProto(p))
	}

	return autoDetected, nil
}

// PrintPortForwards prints the port forwards, optionally filtered to a single connection.
func (client *LocalClient) PrintPortForwards(ctx context.Context, connectionName string) error {
	req := &graftv1.ListConnectionsRequest{}

	sessPID, sessPIDOk := client.sessionPID()
	if sessPIDOk {
		req.Pid = sessPID
	}

	resp, err := client.ListConnections(ctx, req)
	if err != nil {
		return client.handleError(err)
	}

	connNames := make([]string, 0, len(resp.GetConnections()))
	for name := range resp.GetConnections() {
		if connectionName != "" && name != connectionName {
			continue
		}

		connNames = append(connNames, name)
	}

	slices.Sort(connNames)

	for _, name := range connNames {
		status := resp.GetConnections()[name]
		ports := status.GetPortForwardStatuses()

		if len(ports) == 0 {
			continue
		}

		fmt.Fprintf(client.errWriter, "%s:\n", name)

		for _, pf := range ports {
			label := "auto"
			if pf.GetExplicit() {
				label = "explicit"
			}

			if pf.GetConflict() {
				fmt.Fprintf(client.errWriter, "  %s %s %d -> remote:%d [%s] %s\n",
					color.RedString("CONFLICT"),
					pf.GetProtocol(),
					pf.GetLocalPort(),
					pf.GetRemotePort(),
					label,
					pf.GetConflictReason(),
				)
			} else {
				fmt.Fprintf(client.errWriter, "  %s localhost:%d -> remote:%d [%s]\n",
					pf.GetProtocol(),
					pf.GetLocalPort(),
					pf.GetRemotePort(),
					label,
				)
			}
		}
	}

	return nil
}

// Which prints which connection a command is mapped to.
func (client *LocalClient) Which(ctx context.Context, cmd string) error {
	resp, err := client.SessionWhich(ctx, &graftv1.SessionWhichRequest{
		Pid:     uint64(os.Getppid()), //nolint:gosec // overflow okay
		Command: cmd,
	})
	if err != nil {
		return client.handleError(err)
	}

	fmt.Fprintf(client.errWriter, "%s: %s\n", resp.GetConnectionName(), resp.GetRemotePath())

	return nil
}

// PinConnection pins a connection to the current session.
func (client *LocalClient) PinConnection(ctx context.Context, connName string) error {
	sessPID, ok := client.sessionPID()
	if !ok {
		sessPID = uint64(os.Getppid()) //nolint:gosec // overflow okay
	}

	_, err := client.SessionPinConnection(ctx, &graftv1.SessionPinConnectionRequest{
		Pid:            sessPID,
		ConnectionName: connName,
	})
	if err != nil {
		return client.handleError(err)
	}

	return nil
}

// SelectConnectionForCWD returns the connection matching the current working directory.
func (client *LocalClient) SelectConnectionForCWD(ctx context.Context) (*graftv1.SessionSelectConnectionResponse, error) {
	return client.selectConnection(ctx)
}

func (client *LocalClient) selectConnection(ctx context.Context) (*graftv1.SessionSelectConnectionResponse, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err)
	}

	resp, err := client.SessionSelectConnection(ctx, &graftv1.SessionSelectConnectionRequest{
		Pid: uint64(os.Getppid()), //nolint:gosec // overflow okay
		Cwd: cwd,
	})
	if err != nil {
		return nil, errors.Wrap(err)
	}

	return resp, nil
}

// SelectConnection prints the best connection for session.
func (client *LocalClient) SelectConnection(ctx context.Context) error {
	selectResp, err := client.selectConnection(ctx)
	if err != nil {
		return client.handleError(err)
	}

	// Very similar to status. Maybe better to reuse that proto
	fmt.Fprintf(client.errWriter, "%s", selectResp.GetConnectionName())

	if len(selectResp.GetPathRemappings()) != 0 {
		fmt.Fprint(client.errWriter, ":")
	}

	for _, remapping := range selectResp.GetPathRemappings() {
		fmt.Fprintf(client.errWriter, "\nRemapping %s->%s", remapping.GetFromPrefix(), remapping.GetToPrefix())
	}

	fmt.Fprintln(client.errWriter)

	return nil
}

// DumpLogs prints the logs of a connection.
func (client *LocalClient) DumpLogs(ctx context.Context, connectionName string) error {
	resp, err := client.GraftServiceClient.DumpLogs(ctx, &graftv1.DumpLogsRequest{
		ConnectionName: connectionName,
	})
	if err != nil {
		return client.handleError(err)
	}

	fmt.Fprintf(client.errWriter, "STDOUT:\n %s\n\n", resp.GetStdout())
	fmt.Fprintf(client.errWriter, "STDERR:\n %s\n\n", resp.GetStderr())

	return nil
}

// checkForwardCommands returns a filtered set of commands to forward to a connection. Any
// commands being forwarded that are already in the PATH require user confirmation in order
// to avoid cloberring and confusion.
func checkForwardCommands(commands []string, prefix bool, to string) []string {
	commandsToFwd := make([]string, 0, len(commands))

	for _, command := range commands {
		_ = prefix
		_ = to
		// TODO(erd): replace this later if there's a v2 of huh to use
		// checkCmd := command
		// if prefix {
		// 	checkCmd = fmt.Sprintf("%s-%s", to, checkCmd)
		// }
		// if path, err := exec.LookPath(checkCmd); err == nil {
		// 	var confirm bool
		// 	//nolint:errcheck
		// 	huh.NewConfirm().
		// 		Title(fmt.Sprintf("this will override %s, is that okay?", path)).
		// 		Affirmative("Yes").
		// 		Negative("No, skip it").
		// 		Value(&confirm).
		// 		Run()

		// 	if !confirm {
		// 		continue
		// 	}
		// }
		commandsToFwd = append(commandsToFwd, command)
	}

	return commandsToFwd
}

// sortConnectionNames sorts connection names with the current connection first,
// then alphabetically.
func sortConnectionNames(names []string, connections map[string]*graftv1.ConnectionStatus) {
	slices.SortFunc(names, func(a, b string) int {
		aCurrent := connections[a].GetCurrent()

		bCurrent := connections[b].GetCurrent()
		if aCurrent != bCurrent {
			if aCurrent {
				return -1
			}

			return 1
		}

		return strings.Compare(a, b)
	})
}

func connectionStateDisplayString(state graftv1.ConnectionState) string {
	switch state { //nolint:exhaustive
	case graftv1.ConnectionState_CONNECTION_STATE_INITIALIZING:
		return color.YellowString("Initializing")
	case graftv1.ConnectionState_CONNECTION_STATE_CONNECTED:
		return color.GreenString("Connected")
	case graftv1.ConnectionState_CONNECTION_STATE_FAILED:
		return color.RedString("Failed")
	case graftv1.ConnectionState_CONNECTION_STATE_CLOSED:
		return color.HiBlackString("Closed")
	case graftv1.ConnectionState_CONNECTION_STATE_RECONNECTING:
		return color.YellowString("Reconnecting")
	default:
		return color.RedString("Unknown")
	}
}

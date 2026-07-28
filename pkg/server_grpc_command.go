package graft

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

var errUnknownSignal = errors.NewBare("do not know how to handle singal")

// RunCommand runs the given command on the daemon (locally or forwarded to remote) and exposes std[in/out/err] streams to use.
//
// Additionally, the client can control tty information like terminal resizing.
//
// The stream must open with either a StartCommand (start a new command) or an
// AttachCommand (re-attach to a managed command, replaying buffered output).
func (srv *Server) RunCommand(server graftv1.GraftService_RunCommandServer) error {
	req, err := server.Recv()
	if err != nil {
		return errors.Wrap(err)
	}

	switch data := req.GetData().(type) {
	case *graftv1.RunCommandRequest_Start:
		return srv.runCommandFromStart(server, data.Start)
	case *graftv1.RunCommandRequest_Attach:
		return srv.attachCommand(server, data.Attach)
	default:
		return errors.New("first stage of RunCommand must be start or attach")
	}
}

func (srv *Server) runCommandFromStart(
	server graftv1.GraftService_RunCommandServer,
	startReqStart *graftv1.StartCommand,
) error {
	if startReqStart.GetShell() && startReqStart.GetCommand() != "" {
		return errors.New("can either start a shell or run a command")
	}

	runLocally := startReqStart.GetPid() == 0 && startReqStart.GetConnectionName() == ""

	if runLocally {
		if srv.role != ServerRoleRemote {
			return errors.New("not allowed to run a local command")
		}

		slog.DebugContext(server.Context(), "run local command", "req", startReqStart)

		return srv.runManagedLocalCommand(server, startReqStart)
	}

	// TODO(erd): Consider standardizing CWD handling via request headers earlier in the processing pipeline.
	updateErr := srv.sessMgr.UpdateSessionCWD(server.Context(),
		startReqStart.GetPid(), startReqStart.GetCwd())
	if updateErr != nil {
		return updateErr
	}

	runningCmd, err := srv.sessMgr.RunCommand(
		server.Context(),
		startReqStart.GetPid(),
		startReqStart.GetConnectionName(),
		startReqStart.GetShell(),
		startReqStart.GetExactCommand(),
		startReqStart.GetCommand(),
		startReqStart.GetArguments(),
		startReqStart.GetExtraEnv(),
		startReqStart.GetSudo(),
		startReqStart.GetAllocatePty(),
		startReqStart.GetRedirectStdout(),
		startReqStart.GetRedirectStderr(),
		startReqStart.GetPersistence(),
	)
	if err != nil {
		slog.InfoContext(server.Context(), "error running user command", "error", err)

		return err
	}

	started := &graftv1.CommandStarted{}
	if remoteCmd, ok := runningCmd.(*RemoteRunningCommand); ok {
		started.CommandId = remoteCmd.CommandID()
		started.ConnectionName = remoteCmd.ConnectionName()
	}

	if sendErr := server.Send(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Started{
			Started: started,
		},
	}); sendErr != nil {
		runningCmd.Release()

		return errors.Wrap(sendErr)
	}

	slog.DebugContext(server.Context(), "handling running command")
	err = srv.handleRunningCommand(runningCmd, server)
	slog.DebugContext(server.Context(), "handled running command", "error", err)

	return err
}

// runManagedLocalCommand is the remote daemon's start path: the spawned
// command is owned by the command registry (which drains its output and
// applies its disconnect policy) and this stream becomes its first attachment.
func (srv *Server) runManagedLocalCommand(
	server graftv1.GraftService_RunCommandServer,
	startReqStart *graftv1.StartCommand,
) error {
	runningCmd, err := srv.runLocalCommand(server.Context(), startReqStart)
	if err != nil {
		return err
	}

	managed, err := srv.cmdRegistry.Register(runningCmd, ManagedCommandSpec{
		Display: commandDisplay(startReqStart),
		CWD:     startReqStart.GetCwd(),
		Pty:     startReqStart.GetAllocatePty(),
		Persistence: resolvePersistence(
			startReqStart.GetPersistence(),
			startReqStart.GetAllocatePty(),
			startReqStart.GetShell(),
		),
	})
	if err != nil {
		// The registry only refuses when shutting down; don't leak the process.
		if sigErr := runningCmd.Signal(SignalKill); sigErr != nil {
			slog.DebugContext(server.Context(), "error killing unregistered command", "error", sigErr)
		}

		_, _ = runningCmd.Wait() //nolint:errcheck // best-effort cleanup of an unregistered command
		runningCmd.Release()

		return err
	}

	handle := managed.attach()

	if sendErr := server.Send(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Started{
			Started: &graftv1.CommandStarted{
				CommandId:   managed.ID(),
				Persistence: managed.currentPersistence().proto(),
			},
		},
	}); sendErr != nil {
		managed.ClientGone(handle)

		return errors.Wrap(sendErr)
	}

	return srv.serveManagedCommand(server, managed, handle, 0, 0)
}

// commandDisplay renders a start request as a human-readable command line.
func commandDisplay(startReqStart *graftv1.StartCommand) string {
	if startReqStart.GetShell() {
		return "shell"
	}

	parts := append([]string{startReqStart.GetCommand()}, startReqStart.GetArguments()...)

	return strings.Join(parts, " ")
}

// attachCommand re-attaches a client to a managed command: directly from the
// registry on a remote daemon, forwarded over the resolved connection on a
// local daemon.
func (srv *Server) attachCommand(
	server graftv1.GraftService_RunCommandServer,
	attachReq *graftv1.AttachCommand,
) error {
	if srv.role == ServerRoleRemote {
		return srv.attachManagedCommand(server, attachReq)
	}

	return srv.forwardAttachCommand(server, attachReq)
}

func (srv *Server) attachManagedCommand(
	server graftv1.GraftService_RunCommandServer,
	attachReq *graftv1.AttachCommand,
) error {
	managed, ok := srv.cmdRegistry.Get(attachReq.GetCommandId())
	if !ok {
		return errors.Wrap(status.Error(codes.NotFound, "no such command: "+attachReq.GetCommandId()))
	}

	handle := managed.attach()

	// The requested offsets confirm everything below them was consumed;
	// lossless (kill-policy) writers may reuse that space.
	managed.AckOutput(attachReq.GetStdoutOffset(), attachReq.GetStderrOffset())

	// Replay can only start at data the ring still retains.
	stdoutOffset := max(attachReq.GetStdoutOffset(), managed.outRing().StartOffset())
	stderrOffset := max(attachReq.GetStderrOffset(), managed.errRing().StartOffset())
	_, exited, _ := managed.ExitStatus() //nolint:errcheck // liveness peek only

	if sendErr := server.Send(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Attached{
			Attached: &graftv1.CommandAttached{
				CommandId:          managed.ID(),
				Pty:                managed.Pty(),
				StdoutReplayOffset: stdoutOffset,
				StderrReplayOffset: stderrOffset,
				Running:            !exited,
				Persistence:        managed.currentPersistence().proto(),
			},
		},
	}); sendErr != nil {
		managed.ClientGone(handle)

		return errors.Wrap(sendErr)
	}

	return srv.serveManagedCommand(server, managed, handle, stdoutOffset, stderrOffset)
}

// forwardAttachCommand routes an attach from a local daemon to the connection
// owning the command.
func (srv *Server) forwardAttachCommand(
	server graftv1.GraftService_RunCommandServer,
	attachReq *graftv1.AttachCommand,
) error {
	conn, err := srv.findCommandConnection(server.Context(), attachReq.GetConnectionName(), attachReq.GetCommandId())
	if err != nil {
		return err
	}

	runningCmd, attached, err := conn.AttachCommand(
		server.Context(),
		attachReq.GetCommandId(),
		attachReq.GetStdoutOffset(),
		attachReq.GetStderrOffset(),
	)
	if err != nil {
		return err
	}

	if sendErr := server.Send(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Attached{Attached: attached},
	}); sendErr != nil {
		runningCmd.Release()

		return errors.Wrap(sendErr)
	}

	return srv.handleRunningCommand(runningCmd, server)
}

// serveManagedCommand streams a managed command to one attached client:
// output is tailed from the command's rings starting at the given offsets,
// and stdin/out-of-band messages are forwarded in. It returns when the
// command exits (delivering the exit status), the client goes away (the
// command's persistence policy is applied), or another client steals the
// attachment.
func (srv *Server) serveManagedCommand(
	server graftv1.GraftService_RunCommandServer,
	managed *ManagedCommand,
	handle *commandAttachment,
	stdoutOffset, stderrOffset uint64,
) error {
	defer managed.ClientGone(handle)

	stop := make(chan struct{})

	var stopOnce sync.Once

	closeStop := func() { stopOnce.Do(func() { close(stop) }) }
	defer closeStop()

	// Stop tailing when the client stream dies or the command is stolen.
	go func() {
		select {
		case <-server.Context().Done():
		case <-handle.Canceled():
		case <-stop:
		}

		closeStop()
	}()

	// Forward stdin and out-of-band messages into the command.
	// TODO(erd): add to global active goroutines since we can't interrupt recv
	go forwardManagedCommandInput(server, managed, handle, closeStop)

	var sendMu sync.Mutex

	sendResp := func(resp *graftv1.RunCommandResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()

		return server.Send(resp)
	}

	tailManagedCommandOutput(managed, sendResp, stdoutOffset, stderrOffset, stop, closeStop)

	select {
	case <-stop:
		// The tail was interrupted before the command's output completed.
		select {
		case <-handle.Canceled():
			return errors.Wrap(status.Error(codes.Aborted, "command was attached from another client"))
		default:
			// The stream broke without a takeover: report the context cause
			// when there is one (client canceled) or a plain error when the
			// send side failed first.
			if cause := context.Cause(server.Context()); cause != nil {
				return errors.WrapPrefix(cause, "client stream closed")
			}

			return errors.New("client stream closed")
		}
	default:
	}

	// Both rings reached EOF: the command exited; wait for the exit record.
	<-managed.Done()

	exitStatus, _, exitErr := managed.ExitStatus()
	if exitErr != nil {
		srv.cmdRegistry.Remove(managed.ID())

		return errors.Wrap(exitErr)
	}

	if sendErr := sendResp(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_ExitStatus{ExitStatus: int64(exitStatus)},
	}); sendErr != nil {
		// Undelivered exit status: keep the command listed for a later attach.
		return errors.Wrap(sendErr)
	}

	srv.cmdRegistry.Remove(managed.ID())

	return nil
}

// forwardManagedCommandInput forwards a client's stdin and out-of-band
// messages into a managed command until the stream half-closes (graceful "no
// more input"), breaks (client gone; stops the serve), or the attachment is
// stolen.
func forwardManagedCommandInput(
	server graftv1.GraftService_RunCommandServer,
	managed *ManagedCommand,
	handle *commandAttachment,
	closeStop func(),
) {
	for {
		req, err := server.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Graceful half-close: no more input but keep serving output.
				// Whether the command's stdin actually closes is a policy
				// question (keep commands stay attachable with usable stdin).
				if closeErr := managed.CloseStdinFromClient(); closeErr != nil {
					slog.DebugContext(server.Context(), "error closing stdin", "error", closeErr)
				}

				return
			}

			closeStop()

			return
		}

		// A stolen attachment must not keep feeding input; its stale bytes
		// would interleave with the new client's.
		if !managed.currentIs(handle) {
			return
		}

		switch data := req.GetData().(type) {
		case *graftv1.RunCommandRequest_Stdin:
			if _, err := managed.Stdin().Write(data.Stdin); err != nil {
				slog.DebugContext(server.Context(), "error doing stdin write", "error", err)
			}
		case *graftv1.RunCommandRequest_Signal:
			if err := managed.Signal(data.Signal); err != nil {
				slog.DebugContext(server.Context(), "error doing signal", "error", err)
			}
		case *graftv1.RunCommandRequest_EnvVar:
			if err := managed.SetEnvVar(data.EnvVar.GetKey(), data.EnvVar.GetValue()); err != nil {
				slog.DebugContext(server.Context(), "error doing env var", "error", err)
			}
		case *graftv1.RunCommandRequest_WindowChange:
			if err := managed.NotifyWindowChange(int(data.WindowChange.GetHeight()), int(data.WindowChange.GetWidth())); err != nil {
				slog.DebugContext(server.Context(), "error doing window change", "error", err)
			}
		case *graftv1.RunCommandRequest_Ack:
			managed.AckOutput(data.Ack.GetStdoutOffset(), data.Ack.GetStderrOffset())
		}
	}
}

// tailManagedCommandOutput streams both ring buffers to the client from the
// given offsets, returning once both rings hit EOF (command exited and
// drained) or the tail is stopped.
func tailManagedCommandOutput(
	managed *ManagedCommand,
	sendResp func(*graftv1.RunCommandResponse) error,
	stdoutOffset, stderrOffset uint64,
	stop chan struct{},
	closeStop func(),
) {
	var pumps sync.WaitGroup

	pump := func(ring *outputRing, offset uint64, makeResp func([]byte) *graftv1.RunCommandResponse) {
		defer pumps.Done()

		for {
			data, gotOffset, err := ring.ReadAt(offset, stop)
			if err != nil {
				return
			}

			if gotOffset != offset {
				// The client fell so far behind that the ring evicted data it
				// had not seen. The stream cannot express that gap mid-flight,
				// so end it: a re-attach negotiates explicit replay offsets
				// (with the gap accounted for) instead of silently
				// misaligning the client's offset tracking.
				slog.Debug("attached client outrun by command output; ending stream for re-attach",
					"expected_offset", offset, "available_offset", gotOffset)
				closeStop()

				return
			}

			offset = gotOffset + uint64(len(data))

			if sendErr := sendResp(makeResp(data)); sendErr != nil {
				closeStop()

				return
			}
		}
	}

	pumps.Add(2)

	go pump(managed.outRing(), stdoutOffset, func(data []byte) *graftv1.RunCommandResponse {
		return &graftv1.RunCommandResponse{Data: &graftv1.RunCommandResponse_Stdout{Stdout: data}}
	})
	go pump(managed.errRing(), stderrOffset, func(data []byte) *graftv1.RunCommandResponse {
		return &graftv1.RunCommandResponse{Data: &graftv1.RunCommandResponse_Stderr{Stderr: data}}
	})

	pumps.Wait()
}

// RunCommandGRPCServerHandler processes a running command on the server side of a RunCommand. That means
// the [RunningCommand] is coming from another daemon being forwarded by this one.
type RunCommandGRPCServerHandler struct {
	runServer    graftv1.GraftService_RunCommandServer
	runningCmd   RunningCommand
	inputReaders sync.WaitGroup
	sendMu       sync.Mutex
}

// Serve processes the command by forwarding stdin/out/err/oob back and forth until the command is finished.
func (h *RunCommandGRPCServerHandler) Serve(ctx context.Context) error {
	defer h.runningCmd.Release()

	h.handleInputStreams()
	h.handleOutputStream(ctx)

	waitStatus, waitErr := h.runningCmd.Wait()
	slog.DebugContext(ctx, "done serving command", "status", waitStatus, "error", waitErr)

	// unblock anything waiting on stdin
	h.runningCmd.Stdin().Close()

	// wait to process stdout/err
	h.inputReaders.Wait()

	if waitErr == nil {
		sendErr := h.runServer.Send(&graftv1.RunCommandResponse{
			Data: &graftv1.RunCommandResponse_ExitStatus{
				ExitStatus: int64(waitStatus),
			},
		})
		if sendErr != nil {
			slog.ErrorContext(ctx, "error sending exit status", "error", sendErr)
		}

		return nil
	}

	return errors.Wrap(waitErr)
}

func (h *RunCommandGRPCServerHandler) handleInputStreams() {
	// simlply forward both stdout and stderr
	if h.runningCmd.Stdout() != nil {
		h.inputReaders.Go(func() { h.handleReadStream(h.runningCmd.Stdout(), true) })
	}

	if h.runningCmd.Stdout() != h.runningCmd.Stderr() {
		// dont read from stdout twice
		h.inputReaders.Go(func() { h.handleReadStream(h.runningCmd.Stderr(), false) })
	}
}

func (h *RunCommandGRPCServerHandler) handleOutputStream(ctx context.Context) {
	// TODO(erd): add to global active goroutines since we can't interrupt recv
	// forward stdin but also any signals/env-vars/window-changes
	go func() {
		defer h.runningCmd.Stdin().Close()

		for {
			req, err := h.runServer.Recv()
			if err != nil {
				return
			}

			switch data := req.GetData().(type) {
			case *graftv1.RunCommandRequest_Stdin:
				if _, err := h.runningCmd.Stdin().Write(data.Stdin); err != nil {
					slog.ErrorContext(ctx, "error doing stdin write", "error", err)
					// TODO(erd): Determine if this error should be reported to the client via the gRPC stream.
					return
				}
			case *graftv1.RunCommandRequest_Signal:
				err := h.runningCmd.Signal(data.Signal)
				if err != nil {
					slog.ErrorContext(ctx, "error doing signal", "error", err)
					// TODO(erd): Determine if this error should be reported to the client via the gRPC stream.
					// TODO(erd): Evaluate whether returning here is correct or if error should propagate to client.
					return
				}
			case *graftv1.RunCommandRequest_EnvVar:
				err := h.runningCmd.SetEnvVar(data.EnvVar.GetKey(), data.EnvVar.GetValue())
				if err != nil {
					slog.ErrorContext(ctx, "error doing env var", "error", err)
					// TODO(erd): Determine if this error should be reported to the client via the gRPC stream.
					// TODO(erd): Evaluate whether returning here is correct or if error should propagate to client.
					return
				}
			case *graftv1.RunCommandRequest_WindowChange:
				err := h.runningCmd.NotifyWindowChange(int(data.WindowChange.GetHeight()), int(data.WindowChange.GetWidth()))
				if err != nil {
					slog.ErrorContext(ctx, "error doing window change", "error", err)
					// TODO(erd): Determine if this error should be reported to the client via the gRPC stream.
					// TODO(erd): Evaluate whether returning here is correct or if error should propagate to client.
					return
				}
			}
		}
	}()
}

func (h *RunCommandGRPCServerHandler) handleReadStream(reader io.Reader, stdout bool) {
	var buf [1024]byte

	for {
		n, err := reader.Read(buf[:])
		if err != nil {
			return
		}

		var resp *graftv1.RunCommandResponse

		data := buf[:n]
		if stdout {
			resp = &graftv1.RunCommandResponse{
				Data: &graftv1.RunCommandResponse_Stdout{
					Stdout: data,
				},
			}
		} else {
			resp = &graftv1.RunCommandResponse{
				Data: &graftv1.RunCommandResponse_Stderr{
					Stderr: data,
				},
			}
		}

		h.sendMu.Lock()

		if err := h.runServer.Send(resp); err != nil {
			h.sendMu.Unlock()

			return
		}

		h.sendMu.Unlock()
	}
}

// handleRunningCommand is a bidirectional forwarder of a running command. This gets used by a local client to a local daemon
// as well as a local daemon to a remote daemon (by way of a Connection).
// TODO(erd): simplify this; it's handling a running command (local/remote) but as a server (so the remote one is like a client).
func (srv *Server) handleRunningCommand(runningCmd RunningCommand, runServer graftv1.GraftService_RunCommandServer) error {
	runCtx := runServer.Context()
	handler := RunCommandGRPCServerHandler{runServer: runServer, runningCmd: runningCmd}

	return handler.Serve(runCtx)
}

// runLocalCommand is the last mile for running command. This should only ever run on the remote daemon. Given the request
// it will return a running command / shell. By the time this is run, it should be as if the command/shell is being run
// from a local tty.
func (srv *Server) runLocalCommand(ctx context.Context, cmdReq *graftv1.StartCommand) (*LocalRunningCommand, error) {
	shellPath, err := findShellPath()
	if err != nil {
		return nil, err
	}

	// For non-shell commands, prepend a DEBUG trap that re-evaluates env
	// managers (e.g. mise) before every simple command. This makes env
	// activation directory-aware even in compound commands like
	// "cd / && which go" without fragile command string parsing.
	var shellHookPrefix string
	if !cmdReq.GetShell() && srv.envProviders != nil {
		shellHookPrefix = srv.envProviders.ShellHookPrefix()
	}

	var cmd []string
	if cmdReq.GetShell() {
		cmd = makeShellCommand(shellPath, cmdReq.GetCwd())
	} else {
		cmd = makeCommandWrappedInShell(shellPath, cmdReq.GetCwd(), cmdReq.GetCommand(), cmdReq.GetArguments(), cmdReq.GetSudo(), shellHookPrefix)
	}

	var extraEnv []string

	srv.serverMu.Lock()

	sockPath := srv.sshAuthSockPaths[cmdReq.GetOriginConnectionName()]

	srv.serverMu.Unlock()

	if sockPath != "" {
		extraEnv = append(extraEnv, "SSH_AUTH_SOCK="+sockPath)

		// TODO(erd): this is definitely better expressed by the user
		extraEnv = append(extraEnv, `GIT_CONFIG_COUNT=1`)
		extraEnv = append(extraEnv, `GIT_CONFIG_KEY_0=url.ssh://git@github.com/.insteadOf`)
		extraEnv = append(extraEnv, `GIT_CONFIG_VALUE_0=https://github.com/`)
	}

	// Set trust env for all commands (including shells) so mise configs
	// in connection root directories are auto-trusted.
	if srv.envProviders != nil {
		extraEnv = append(extraEnv, srv.envProviders.TrustEnv()...)
	}

	extraEnv = append(extraEnv, cmdReq.GetExtraEnv()...)

	return ExecuteLocalCommand(
		ctx,
		cmd,
		cmdReq.GetAllocatePty(),
		cmdReq.GetRedirectStdout(),
		cmdReq.GetRedirectStderr(),
		extraEnv...,
	)
}

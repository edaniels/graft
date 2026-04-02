package graft

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

var errUnknownSignal = errors.NewBare("do not know how to handle singal")

// RunCommand runs the given command on the daemon (locally or forwarded to remote) and exposes std[in/out/err] streams to use.
//
// Additionally, the client can control tty information like terminal resizing.
func (srv *Server) RunCommand(server graftv1.GraftService_RunCommandServer) error {
	req, err := server.Recv()
	if err != nil {
		return errors.Wrap(err)
	}

	startReq, ok := req.GetData().(*graftv1.RunCommandRequest_Start)
	if !ok {
		return errors.New("first stage of RunCommand must be start")
	}

	startReqStart := startReq.Start
	if startReqStart.GetShell() && startReqStart.GetCommand() != "" {
		return errors.New("can either start a shell or run a command")
	}

	var runningCmd RunningCommand

	runLocally := startReqStart.GetPid() == 0 && startReqStart.GetConnectionName() == ""

	if runLocally {
		if srv.role != ServerRoleRemote {
			return errors.New("not allowed to run a local command")
		}

		slog.DebugContext(server.Context(), "run local command", "req", startReqStart)

		runningCmd, err = srv.runLocalCommand(server.Context(), startReqStart)
		if err != nil {
			return err
		}
	} else {
		// TODO(erd): Consider standardizing CWD handling via request headers earlier in the processing pipeline.
		updateErr := srv.sessMgr.UpdateSessionCWD(server.Context(),
			startReqStart.GetPid(), startReqStart.GetCwd())
		if updateErr != nil {
			return updateErr
		}

		runningCmd, err = srv.sessMgr.RunCommand(
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
		)
		if err != nil {
			slog.InfoContext(server.Context(), "error running user command", "error", err)

			return err
		}
	}

	if sendErr := server.Send(&graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Started{
			Started: &graftv1.CommandStarted{},
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

// RunCommandGRPCServerHandler processes a running command on the server side of a RunCommand. That means
// the [RunningCommand] is coming from either a local command or another daemon.
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

	if srv.sshAuthSockPath != "" {
		extraEnv = append(extraEnv, "SSH_AUTH_SOCK="+srv.sshAuthSockPath)

		// TODO(erd): this is definitely better expressed by the user
		extraEnv = append(extraEnv, `GIT_CONFIG_COUNT=1`)
		extraEnv = append(extraEnv, `GIT_CONFIG_KEY_0=url.ssh://git@github.com/.insteadOf`)
		extraEnv = append(extraEnv, `GIT_CONFIG_VALUE_0=https://github.com/`)
	}

	srv.serverMu.Unlock()

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

package graft

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// A RemoteRunningCommand is a command running over gRPC that started by a client's request.
// Each input/output stream is reflected by gRPC sends/recvs. Out-of-band gRPC messages are used
// for signal, env var changes, and window changes.
type RemoteRunningCommand struct {
	runClient grpc.BidiStreamingClient[graftv1.RunCommandRequest, graftv1.RunCommandResponse]

	stdinPipeR  io.ReadCloser
	stdinPipeW  io.WriteCloser
	stdoutPipeR io.ReadCloser
	stdoutPipeW io.WriteCloser
	stderrPipeR io.ReadCloser
	stderrPipeW io.WriteCloser

	cmdDone    chan commandDoneStatus
	processing sync.WaitGroup
}

type commandDoneStatus struct {
	ExitStatus int
	Err        error
}

// NewRemoteRunningCommand returns and starts processing a running command originating from a gRPC client
// request stream.
func NewRemoteRunningCommand(
	runClient grpc.BidiStreamingClient[graftv1.RunCommandRequest, graftv1.RunCommandResponse],
) *RemoteRunningCommand {
	// establish pipes - we have a read/write side for each since this command is forwarding
	// from a client (the on the terminal) and a server (the one running the process)

	// stdin:
	// consumer - writes to write side
	// client - consumes read side
	// server - receives sends from client's reads
	stdinPipeR, stdinPipeW := io.Pipe()
	// stdout:
	// consumer - reads from read side
	// client - receives from server and writes to write side
	// server - sends to client's receive side
	stdoutPipeR, stdoutPipeW := io.Pipe()
	// stderr:
	// consumer - reads from read side
	// client - receives from server and writes to write side
	// server - sends to client's receive side
	stderrPipeR, stderrPipeW := io.Pipe()

	cmd := &RemoteRunningCommand{
		runClient:   runClient,
		stdinPipeR:  stdinPipeR,
		stdinPipeW:  stdinPipeW,
		stdoutPipeR: stdoutPipeR,
		stdoutPipeW: stdoutPipeW,
		stderrPipeR: stderrPipeR,
		stderrPipeW: stderrPipeW,
		cmdDone:     make(chan commandDoneStatus),
	}

	cmd.processing.Go(cmd.process)

	return cmd
}

func (rc *RemoteRunningCommand) Stdin() io.WriteCloser {
	return rc.stdinPipeW
}

func (rc *RemoteRunningCommand) Stdout() io.Reader {
	return rc.stdoutPipeR
}

func (rc *RemoteRunningCommand) Stderr() io.Reader {
	return rc.stderrPipeR
}

func (rc *RemoteRunningCommand) Signal(sig string) error {
	if err := rc.runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Signal{
			Signal: sig,
		},
	}); err != nil {
		return errors.WrapPrefix(err, "error sending signal over gRPC")
	}

	return nil
}

// Wait simply blocks on the underlying processing to finish and returns either the exit status
// or an unexpected error.
func (rc *RemoteRunningCommand) Wait() (int, error) {
	defer rc.processing.Wait()

	doneStatus := <-rc.cmdDone
	if doneStatus.Err != nil {
		return -1, doneStatus.Err
	}

	return doneStatus.ExitStatus, nil
	// TODO(erd): Verify that removing context handling from Wait doesn't affect cancellation.
}

// Release does nothing to cleanup resources since it is handled in process.
func (rc *RemoteRunningCommand) Release() {
}

func (rc *RemoteRunningCommand) SetEnvVar(key, value string) error {
	if err := rc.runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_EnvVar{
			EnvVar: &graftv1.SetEnvVar{
				Key:   key,
				Value: value,
			},
		},
	}); err != nil {
		return errors.WrapPrefix(err, "error setting env var over gRPC")
	}

	return nil
}

func (rc *RemoteRunningCommand) NotifyWindowChange(h, w int) error {
	if err := rc.runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_WindowChange{
			WindowChange: &graftv1.WindowChange{
				Height: int64(h),
				Width:  int64(w),
			},
		},
	}); err != nil {
		return errors.WrapPrefix(err, "error notifying window change over gRPC")
	}

	return nil
}

// process reads from stdout/err into the pipes and writes to gRPC for stdin and out-of-band
// messages until the process exits (which may be encouraged by stdin closing but not always).
func (rc *RemoteRunningCommand) process() {
	defer rc.stdoutPipeW.Close()
	defer rc.stderrPipeW.Close()

	var wg sync.WaitGroup
	defer wg.Wait()

	// Read from local stdin and write to remote stdin
	rc.processing.Go(func() {
		defer func() {
			err := rc.runClient.CloseSend()
			if err != nil {
				slog.ErrorContext(context.Background(), "unlikely: error closing send", "error", err)
			}
		}()

		// Note(erd): does this matter?
		var buf [1024]byte

		for {
			n, err := rc.stdinPipeR.Read(buf[:])
			if err != nil {
				err := rc.runClient.CloseSend()
				if err != nil {
					slog.ErrorContext(context.Background(), "unlikely: error closing send", "error", err)
				}

				return
			}

			if err := rc.runClient.Send(&graftv1.RunCommandRequest{
				Data: &graftv1.RunCommandRequest_Stdin{
					Stdin: buf[:n],
				},
			}); err != nil {
				return
			}
		}
	})

	defer rc.stdinPipeW.Close() // unblock the above which may leave some data in the stdin buffer

	// Read from stdout/err/exit and write to corresponding pipes
	for {
		// TODO(erd): Verify that removing context handling from Wait doesn't affect cancellation.
		resp, err := rc.runClient.Recv()
		if err != nil {
			rc.cmdDone <- commandDoneStatus{Err: err}

			return
		}

		switch data := resp.GetData().(type) {
		case *graftv1.RunCommandResponse_Stdout:
			if _, writeErr := rc.stdoutPipeW.Write(data.Stdout); writeErr != nil {
				rc.cmdDone <- commandDoneStatus{Err: writeErr}

				return
			}
		case *graftv1.RunCommandResponse_Stderr:
			if _, writeErr := rc.stderrPipeW.Write(data.Stderr); writeErr != nil {
				rc.cmdDone <- commandDoneStatus{Err: writeErr}

				return
			}
		case *graftv1.RunCommandResponse_ExitStatus:
			rc.cmdDone <- commandDoneStatus{ExitStatus: int(data.ExitStatus)}

			return
		case *graftv1.RunCommandResponse_Started:
			// Safety net: Started should already be consumed before process() runs,
			// but ignore it if it arrives here.
			continue
		}
	}
}

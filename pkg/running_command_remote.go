package graft

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// runCommandStreamClient is the client side of a RunCommand stream.
type runCommandStreamClient = grpc.BidiStreamingClient[graftv1.RunCommandRequest, graftv1.RunCommandResponse]

// reattachFunc opens a fresh RunCommand stream attached to a managed command,
// resuming output from the given absolute offsets. Each call is a single
// attempt; callers retry with backoff.
type reattachFunc func(ctx context.Context, stdoutOffset, stderrOffset uint64) (
	runCommandStreamClient, *graftv1.CommandAttached, error,
)

const (
	reattachInitialBackoff = time.Second
	reattachMaxBackoff     = 15 * time.Second

	// commandAckEveryBytes is how much newly delivered output accrues before
	// the daemon is sent a consumption ack. It must stay well under the
	// daemon's output ring capacity or lossless (kill-policy) commands would
	// stall waiting for confirmation.
	commandAckEveryBytes = 128 << 10
)

// A RemoteRunningCommand is a command running over gRPC that started by a client's request.
// Each input/output stream is reflected by gRPC sends/recvs. Out-of-band gRPC messages are used
// for signal, env var changes, and window changes.
//
// When constructed with a reattach function and command ID, the command is
// resumable: a broken transport does not end it. Instead, output offsets are
// tracked and the stream is re-established against the daemon's managed
// command, replaying anything missed, until the consumer's context ends or
// the daemon reports the command gone.
type RemoteRunningCommand struct {
	commandID string
	connName  string
	reattach  reattachFunc

	// onDeliberateDetach fires when the consumer's context ends while the
	// command still runs: the client left on purpose (as opposed to a
	// transport break), which for kill-policy commands means "clean me up
	// now" rather than waiting out the daemon's re-attach window.
	onDeliberateDetach func()

	//nolint:containedctx // governs resumability; when it ends, so do reattach attempts
	streamCtx context.Context

	stdinPipeR  *io.PipeReader
	stdinPipeW  *io.PipeWriter
	stdoutPipeR *io.PipeReader
	stdoutPipeW *io.PipeWriter
	stderrPipeR *io.PipeReader
	stderrPipeW *io.PipeWriter

	// stateMu guards the current client, swap notification, queued OOB
	// messages, stdin completion, and last known window size.
	stateMu   sync.Mutex
	runClient runCommandStreamClient
	swapped   chan struct{} // closed and replaced when runClient is swapped
	pending   []*graftv1.RunCommandRequest
	stdinDone bool // the consumer half-closed stdin; must be replayed on reattach
	lastWinH  int
	lastWinW  int

	// sendMu serializes all Sends on the current stream (gRPC forbids
	// concurrent SendMsg on one stream).
	sendMu sync.Mutex

	// stdoutOffset/stderrOffset count bytes delivered to the consumer,
	// lastAckedTotal tracks how much of that has been confirmed back to the
	// daemon, and attachCancel tears down the stream of the current re-attach
	// attempt; only the process goroutine touches them.
	stdoutOffset   uint64
	stderrOffset   uint64
	lastAckedTotal uint64
	attachCancel   context.CancelFunc

	cmdDone    chan commandDoneStatus
	finished   chan struct{} // closed when processing ends
	processing sync.WaitGroup
}

type commandDoneStatus struct {
	ExitStatus int
	Err        error
}

// NewRemoteRunningCommand returns and starts processing a non-resumable
// running command originating from a gRPC client request stream.
func NewRemoteRunningCommand(
	runClient runCommandStreamClient,
) *RemoteRunningCommand {
	return newRemoteRunningCommand(context.Background(), runClient, "", "", nil, 0, 0, nil)
}

// newResumableRemoteRunningCommand returns and starts processing a running
// command that survives transport breaks by re-attaching to the daemon's
// managed command. The initial offsets say how much output the consumer has
// already seen (non-zero when the stream itself began as an attach).
func newResumableRemoteRunningCommand(
	ctx context.Context,
	runClient runCommandStreamClient,
	commandID string,
	connName string,
	reattach reattachFunc,
	stdoutOffset, stderrOffset uint64,
	onDeliberateDetach func(),
) *RemoteRunningCommand {
	return newRemoteRunningCommand(ctx, runClient, commandID, connName, reattach, stdoutOffset, stderrOffset, onDeliberateDetach)
}

func newRemoteRunningCommand(
	ctx context.Context,
	runClient runCommandStreamClient,
	commandID string,
	connName string,
	reattach reattachFunc,
	stdoutOffset, stderrOffset uint64,
	onDeliberateDetach func(),
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
		commandID:          commandID,
		connName:           connName,
		reattach:           reattach,
		onDeliberateDetach: onDeliberateDetach,
		streamCtx:          ctx,
		runClient:          runClient,
		swapped:            make(chan struct{}),
		stdinPipeR:         stdinPipeR,
		stdinPipeW:         stdinPipeW,
		stdoutPipeR:        stdoutPipeR,
		stdoutPipeW:        stdoutPipeW,
		stderrPipeR:        stderrPipeR,
		stderrPipeW:        stderrPipeW,
		stdoutOffset:       stdoutOffset,
		stderrOffset:       stderrOffset,
		// Buffered so process() can park the exit status and run its deferred
		// pipe closes without a Wait()er already present: consumers that
		// drain output to EOF before calling Wait would otherwise deadlock
		// (EOF needs the deferred close, the close needs this send, the send
		// would need Wait). It also lets process() exit when the command is
		// abandoned without Wait ever being called.
		cmdDone:  make(chan commandDoneStatus, 1),
		finished: make(chan struct{}),
	}

	// If the consumer's context can end, tear the pipes down when it does: a
	// consumer that stopped reading would otherwise leave process() wedged in
	// a pipe write forever (and its stream handler with it).
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cause := context.Cause(ctx)
				cmd.stdoutPipeW.CloseWithError(cause)
				cmd.stderrPipeW.CloseWithError(cause)
				cmd.stdinPipeW.Close()

				if cmd.onDeliberateDetach != nil {
					cmd.onDeliberateDetach()
				}
			case <-cmd.finished:
			}
		}()
	}

	cmd.processing.Go(cmd.process)

	return cmd
}

// CommandID returns the daemon-side managed command identifier (empty for
// commands that predate registries).
func (rc *RemoteRunningCommand) CommandID() string {
	return rc.commandID
}

// ConnectionName returns the connection this command runs over.
func (rc *RemoteRunningCommand) ConnectionName() string {
	return rc.connName
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

// currentClient returns the active stream and a channel closed when it is
// replaced by a reattach.
func (rc *RemoteRunningCommand) currentClient() (runCommandStreamClient, <-chan struct{}) {
	rc.stateMu.Lock()
	defer rc.stateMu.Unlock()

	return rc.runClient, rc.swapped
}

// resumable reports whether the command may try to reattach at all.
func (rc *RemoteRunningCommand) resumable() bool {
	return rc.reattach != nil && rc.commandID != ""
}

// send serializes a Send on the current stream.
func (rc *RemoteRunningCommand) send(client runCommandStreamClient, req *graftv1.RunCommandRequest) error {
	rc.sendMu.Lock()
	defer rc.sendMu.Unlock()

	//nolint:wrapcheck // callers decide how to handle/queue
	return client.Send(req)
}

// sendOOB sends an out-of-band message. For resumable commands, a failed send
// during a transport gap is queued and flushed after the next reattach rather
// than surfaced: the caller's stream to us is healthy, ours to the daemon is
// what broke.
func (rc *RemoteRunningCommand) sendOOB(req *graftv1.RunCommandRequest) error {
	for {
		client, _ := rc.currentClient()

		err := rc.send(client, req)
		if err == nil {
			return nil
		}

		if !rc.resumable() {
			return errors.Wrap(err)
		}

		rc.stateMu.Lock()

		if rc.runClient == client {
			// Still the broken stream: park the message for the reattach
			// flush. Queuing is only correct while the failed stream is
			// current - otherwise the flush already ran and would never pick
			// this message up.
			rc.pending = append(rc.pending, req)
			rc.stateMu.Unlock()

			return nil
		}

		rc.stateMu.Unlock()
		// A reattach swapped the stream while we were sending; retry on the
		// fresh one.
	}
}

func (rc *RemoteRunningCommand) Signal(sig string) error {
	if err := rc.sendOOB(&graftv1.RunCommandRequest{
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
}

// Release does nothing to cleanup resources since it is handled in process.
func (rc *RemoteRunningCommand) Release() {
}

func (rc *RemoteRunningCommand) SetEnvVar(key, value string) error {
	if err := rc.sendOOB(&graftv1.RunCommandRequest{
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
	rc.stateMu.Lock()
	rc.lastWinH, rc.lastWinW = h, w
	rc.stateMu.Unlock()

	if err := rc.sendOOB(&graftv1.RunCommandRequest{
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

// setClient installs a freshly attached stream: waiters (like the stdin pump)
// are woken, the last known window size is re-synced, out-of-band messages
// queued during the gap are flushed, and a consumer stdin half-close from
// before the break is replayed.
func (rc *RemoteRunningCommand) setClient(client runCommandStreamClient) {
	rc.stateMu.Lock()
	rc.runClient = client
	close(rc.swapped)
	rc.swapped = make(chan struct{})
	pending := rc.pending
	rc.pending = nil
	stdinDone := rc.stdinDone
	winH, winW := rc.lastWinH, rc.lastWinW
	rc.stateMu.Unlock()

	if winH > 0 && winW > 0 {
		resync := &graftv1.RunCommandRequest{
			Data: &graftv1.RunCommandRequest_WindowChange{
				WindowChange: &graftv1.WindowChange{Height: int64(winH), Width: int64(winW)},
			},
		}
		if err := rc.send(client, resync); err != nil {
			slog.Debug("error re-syncing window size after reattach", "error", err)
		}
	}

	var failed []*graftv1.RunCommandRequest

	for _, req := range pending {
		if err := rc.send(client, req); err != nil {
			slog.Debug("error flushing queued message after reattach", "error", err)

			failed = append(failed, req)
		}
	}

	if len(failed) > 0 {
		// The fresh stream broke already; keep the messages for the next one,
		// ahead of anything queued in the meantime.
		rc.stateMu.Lock()
		rc.pending = append(failed, rc.pending...)
		rc.stateMu.Unlock()
	}

	if stdinDone {
		// The consumer finished its input before the break; the daemon side
		// (e.g. a piped command waiting on EOF) still needs to hear it.
		rc.sendMu.Lock()

		if err := client.CloseSend(); err != nil {
			slog.Debug("error replaying stdin close after reattach", "error", err)
		}

		rc.sendMu.Unlock()
	}
}

// pumpStdin forwards consumer stdin to the daemon. For resumable commands, a
// chunk that fails to send is retained and re-sent once a reattach installs a
// fresh stream; the pipe's backpressure naturally pauses the upstream writer
// in the meantime.
func (rc *RemoteRunningCommand) pumpStdin() {
	closeSend := func() {
		// Record the half-close first so a reattach racing this replays it on
		// the fresh stream.
		rc.stateMu.Lock()
		rc.stdinDone = true
		rc.stateMu.Unlock()

		client, _ := rc.currentClient()

		rc.sendMu.Lock()
		defer rc.sendMu.Unlock()

		if err := client.CloseSend(); err != nil {
			slog.Debug("error closing send side", "error", err)
		}
	}
	defer closeSend()

	// Note(erd): does this matter?
	var buf [1024]byte

	for {
		n, err := rc.stdinPipeR.Read(buf[:])
		if err != nil {
			return
		}

		req := &graftv1.RunCommandRequest{
			Data: &graftv1.RunCommandRequest_Stdin{
				Stdin: buf[:n],
			},
		}

		for {
			client, swapped := rc.currentClient()
			if sendErr := rc.send(client, req); sendErr == nil {
				break
			}

			if !rc.resumable() {
				return
			}

			select {
			case <-swapped:
				// a fresh stream was attached; retry the chunk
			case <-rc.finished:
				return
			}
		}
	}
}

// process reads from stdout/err into the pipes and writes to gRPC for stdin and out-of-band
// messages until the process exits (which may be encouraged by stdin closing but not always).
func (rc *RemoteRunningCommand) process() {
	defer close(rc.finished)
	defer rc.stdoutPipeW.Close()
	defer rc.stderrPipeW.Close()
	defer rc.cancelAttachStream()

	rc.processing.Go(rc.pumpStdin)

	defer rc.stdinPipeW.Close() // unblock the above which may leave some data in the stdin buffer

	for {
		client, _ := rc.currentClient()

		resp, err := client.Recv()
		if err != nil {
			if rc.tryResume(err) {
				continue
			}

			rc.cmdDone <- commandDoneStatus{Err: err}

			return
		}

		switch data := resp.GetData().(type) {
		case *graftv1.RunCommandResponse_Stdout:
			if _, writeErr := rc.stdoutPipeW.Write(data.Stdout); writeErr != nil {
				rc.cmdDone <- commandDoneStatus{Err: writeErr}

				return
			}

			rc.stdoutOffset += uint64(len(data.Stdout))
			rc.maybeAck()
		case *graftv1.RunCommandResponse_Stderr:
			if _, writeErr := rc.stderrPipeW.Write(data.Stderr); writeErr != nil {
				rc.cmdDone <- commandDoneStatus{Err: writeErr}

				return
			}

			rc.stderrOffset += uint64(len(data.Stderr))
			rc.maybeAck()
		case *graftv1.RunCommandResponse_ExitStatus:
			rc.cmdDone <- commandDoneStatus{ExitStatus: int(data.ExitStatus)}

			return
		case *graftv1.RunCommandResponse_Started, *graftv1.RunCommandResponse_Attached:
			// Safety net: handshake messages are consumed before process()
			// runs (and by reattach for resumed streams); ignore stragglers.
			continue
		}
	}
}

// cancelAttachStream releases the previous re-attached stream's context, if
// any. Only called from the process goroutine.
func (rc *RemoteRunningCommand) cancelAttachStream() {
	if rc.attachCancel != nil {
		rc.attachCancel()
		rc.attachCancel = nil
	}
}

// noteReplayGap warns when the daemon can only replay from a later offset
// than the consumer has seen: that many bytes were evicted from the bounded
// replay buffer during the disconnect.
func (rc *RemoteRunningCommand) noteReplayGap(stream string, replayOffset, seenOffset uint64) {
	if replayOffset > seenOffset {
		slog.WarnContext(rc.streamCtx, "output lost during disconnect (evicted from replay buffer)",
			"command_id", rc.commandID, "stream", stream, "bytes", replayOffset-seenOffset)
	}
}

// maybeAck confirms delivered output back to the daemon once enough has
// accrued. The pipe writes above complete only after the consumer read them,
// so the offsets are true consumption, not just receipt. Only called from the
// process goroutine. A failed send is retried implicitly: a reattach
// handshake carries the same offsets.
func (rc *RemoteRunningCommand) maybeAck() {
	if !rc.resumable() {
		return
	}

	total := rc.stdoutOffset + rc.stderrOffset
	if total-rc.lastAckedTotal < commandAckEveryBytes {
		return
	}

	client, _ := rc.currentClient()

	if err := rc.send(client, &graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Ack{
			Ack: &graftv1.CommandAck{
				StdoutOffset: rc.stdoutOffset,
				StderrOffset: rc.stderrOffset,
			},
		},
	}); err == nil {
		rc.lastAckedTotal = total
	}
}

// tryResume attempts to re-establish the command stream after a transport
// failure, retrying with backoff. It reports false when resuming is
// impossible (not resumable, consumer gone, or the daemon says the command is
// gone/stolen), in which case the causing error ends the command.
func (rc *RemoteRunningCommand) tryResume(cause error) bool {
	if !rc.resumable() {
		return false
	}

	// The consumer's own stream ending is a deliberate disconnect, not a
	// transport failure; let the daemon-side persistence policy take it from
	// here.
	if rc.streamCtx.Err() != nil {
		return false
	}

	// NotFound means the command is gone; Aborted means another client
	// deliberately took it over. Neither is resumable.
	if code := status.Code(cause); code == codes.NotFound || code == codes.Aborted {
		return false
	}

	slog.InfoContext(rc.streamCtx, "command stream broke; re-attaching",
		"command_id", rc.commandID, "connection", rc.connName, "error", cause)

	backoff := reattachInitialBackoff

	for {
		// Each attempt gets its own cancelable context so a stream that was
		// opened but failed mid-handshake (or is later replaced) is torn down
		// rather than leaking until the consumer's context ends.
		attemptCtx, attemptCancel := context.WithCancel(rc.streamCtx)

		client, attached, err := rc.reattach(attemptCtx, rc.stdoutOffset, rc.stderrOffset)
		if err == nil {
			rc.noteReplayGap("stdout", attached.GetStdoutReplayOffset(), rc.stdoutOffset)
			rc.noteReplayGap("stderr", attached.GetStderrReplayOffset(), rc.stderrOffset)

			rc.stdoutOffset = max(rc.stdoutOffset, attached.GetStdoutReplayOffset())
			rc.stderrOffset = max(rc.stderrOffset, attached.GetStderrReplayOffset())

			rc.cancelAttachStream()
			rc.attachCancel = attemptCancel
			rc.setClient(client)

			slog.InfoContext(rc.streamCtx, "re-attached to command",
				"command_id", rc.commandID, "connection", rc.connName)

			return true
		}

		attemptCancel()

		if status.Code(err) == codes.NotFound {
			slog.WarnContext(rc.streamCtx, "command no longer exists; giving up re-attach",
				"command_id", rc.commandID)

			return false
		}

		slog.DebugContext(rc.streamCtx, "re-attach attempt failed; retrying",
			"command_id", rc.commandID, "backoff", backoff, "error", err)

		select {
		case <-rc.streamCtx.Done():
			return false
		case <-time.After(backoff):
			backoff = min(backoff*2, reattachMaxBackoff) //nolint:mnd // exponential backoff doubling
		}
	}
}

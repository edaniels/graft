package graft

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

type fakeRecvItem struct {
	resp *graftv1.RunCommandResponse
	err  error
}

// fakeRunStream is an in-memory runCommandStreamClient driven by tests.
type fakeRunStream struct {
	ctx        context.Context //nolint:containedctx // test double
	sendFn     func(*graftv1.RunCommandRequest) error
	recvCh     chan fakeRecvItem
	closeSends chan struct{}
}

func newFakeRunStream(ctx context.Context, sendFn func(*graftv1.RunCommandRequest) error) *fakeRunStream {
	if sendFn == nil {
		sendFn = func(*graftv1.RunCommandRequest) error { return nil }
	}

	return &fakeRunStream{
		ctx:        ctx,
		sendFn:     sendFn,
		recvCh:     make(chan fakeRecvItem, 8),
		closeSends: make(chan struct{}, 8),
	}
}

func (f *fakeRunStream) Send(req *graftv1.RunCommandRequest) error { return f.sendFn(req) }

func (f *fakeRunStream) Recv() (*graftv1.RunCommandResponse, error) {
	item := <-f.recvCh

	return item.resp, item.err
}

func (f *fakeRunStream) Header() (metadata.MD, error) { return metadata.MD{}, nil }
func (f *fakeRunStream) Trailer() metadata.MD         { return nil }
func (f *fakeRunStream) CloseSend() error {
	select {
	case f.closeSends <- struct{}{}:
	default:
	}

	return nil
}
func (f *fakeRunStream) Context() context.Context { return f.ctx }
func (f *fakeRunStream) SendMsg(any) error        { return nil }
func (f *fakeRunStream) RecvMsg(any) error        { return nil }

func stdoutResp(data string) fakeRecvItem {
	return fakeRecvItem{resp: &graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_Stdout{Stdout: []byte(data)},
	}}
}

func exitResp(code int64) fakeRecvItem {
	return fakeRecvItem{resp: &graftv1.RunCommandResponse{
		Data: &graftv1.RunCommandResponse_ExitStatus{ExitStatus: code},
	}}
}

// collectStdout drains a command's stdout into a synchronized buffer.
func collectStdout(rc *RemoteRunningCommand) func() string {
	var (
		mu  sync.Mutex
		out []byte
	)

	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]byte, 1024)

		for {
			n, err := rc.Stdout().Read(buf)

			mu.Lock()

			out = append(out, buf[:n]...)

			mu.Unlock()

			if err != nil {
				return
			}
		}
	}()

	return func() string {
		<-done

		mu.Lock()
		defer mu.Unlock()

		return string(out)
	}
}

func TestRemoteRunningCommandResumeReplaysFromOffsets(t *testing.T) {
	stream1 := newFakeRunStream(t.Context(), nil)
	stream2 := newFakeRunStream(t.Context(), nil)

	reattachOffsets := make(chan [2]uint64, 1)
	reattach := func(_ context.Context, stdoutOffset, stderrOffset uint64) (
		runCommandStreamClient, *graftv1.CommandAttached, error,
	) {
		reattachOffsets <- [2]uint64{stdoutOffset, stderrOffset}

		return stream2, &graftv1.CommandAttached{
			StdoutReplayOffset: stdoutOffset,
			StderrReplayOffset: stderrOffset,
			Running:            true,
		}, nil
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)
	stdout := collectStdout(rc)

	stream1.recvCh <- stdoutResp("AB")

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Unavailable, "transport broke")}

	select {
	case offsets := <-reattachOffsets:
		test.That(t, offsets[0], test.ShouldEqual, 2)
		test.That(t, offsets[1], test.ShouldEqual, 0)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for reattach")
	}

	stream2.recvCh <- stdoutResp("CD")

	stream2.recvCh <- exitResp(7)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 7)
	test.That(t, stdout(), test.ShouldEqual, "ABCD")
}

func TestRemoteRunningCommandNotFoundEndsCommand(t *testing.T) {
	stream1 := newFakeRunStream(t.Context(), nil)

	reattach := func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
		t.Error("reattach must not be consulted for NotFound stream errors")

		return nil, nil, status.Error(codes.NotFound, "gone")
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.NotFound, "no such command")}

	_, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldNotBeNil)
	test.That(t, status.Code(waitErr), test.ShouldEqual, codes.NotFound)
}

func TestRemoteRunningCommandNoResumeAfterConsumerGone(t *testing.T) {
	streamCtx, cancel := context.WithCancel(t.Context())

	stream1 := newFakeRunStream(streamCtx, nil)

	reattach := func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
		t.Error("reattach must not run once the consumer context ended")

		return nil, nil, status.Error(codes.NotFound, "gone")
	}

	rc := newResumableRemoteRunningCommand(streamCtx, stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	// The consumer goes away first; the subsequent stream error is a
	// deliberate disconnect, not something to resume from.
	cancel()

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Canceled, "context canceled")}

	_, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldNotBeNil)
}

func TestRemoteRunningCommandStdinRetriedAfterResume(t *testing.T) {
	firstSendAttempted := make(chan struct{})

	var firstSendOnce sync.Once

	stream1 := newFakeRunStream(t.Context(), func(*graftv1.RunCommandRequest) error {
		firstSendOnce.Do(func() { close(firstSendAttempted) })

		return status.Error(codes.Unavailable, "transport broke")
	})

	sentStdin := make(chan []byte, 8)
	stream2 := newFakeRunStream(t.Context(), func(req *graftv1.RunCommandRequest) error {
		if data, ok := req.GetData().(*graftv1.RunCommandRequest_Stdin); ok {
			sentStdin <- data.Stdin
		}

		return nil
	})

	reattach := func(_ context.Context, stdoutOffset, stderrOffset uint64) (
		runCommandStreamClient, *graftv1.CommandAttached, error,
	) {
		return stream2, &graftv1.CommandAttached{
			StdoutReplayOffset: stdoutOffset,
			StderrReplayOffset: stderrOffset,
			Running:            true,
		}, nil
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	go func() {
		//nolint:errcheck // the write only completes once the pipe is consumed
		rc.Stdin().Write([]byte("hi"))
	}()

	// Only break the recv side once the stdin chunk has hit the dead stream,
	// proving the retained chunk gets re-sent on the fresh one.
	<-firstSendAttempted

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Unavailable, "transport broke")}

	select {
	case data := <-sentStdin:
		test.That(t, string(data), test.ShouldEqual, "hi")
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for stdin to be re-sent after resume")
	}

	stream2.recvCh <- exitResp(0)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
}

func TestRemoteRunningCommandQueuesOOBDuringGap(t *testing.T) {
	stream1 := newFakeRunStream(t.Context(), func(*graftv1.RunCommandRequest) error {
		return status.Error(codes.Unavailable, "transport broke")
	})

	sentSignals := make(chan string, 8)
	stream2 := newFakeRunStream(t.Context(), func(req *graftv1.RunCommandRequest) error {
		if data, ok := req.GetData().(*graftv1.RunCommandRequest_Signal); ok {
			sentSignals <- data.Signal
		}

		return nil
	})

	reattach := func(_ context.Context, stdoutOffset, stderrOffset uint64) (
		runCommandStreamClient, *graftv1.CommandAttached, error,
	) {
		return stream2, &graftv1.CommandAttached{
			StdoutReplayOffset: stdoutOffset,
			StderrReplayOffset: stderrOffset,
			Running:            true,
		}, nil
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	// The transport is down: the signal cannot be delivered now but must not
	// error; it is queued for the next attach.
	test.That(t, rc.Signal(SignalTerminate), test.ShouldBeNil)

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Unavailable, "transport broke")}

	select {
	case sig := <-sentSignals:
		test.That(t, sig, test.ShouldEqual, SignalTerminate)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for queued signal after resume")
	}

	stream2.recvCh <- exitResp(0)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
}

func TestRemoteRunningCommandSequentialDrainThenWait(t *testing.T) {
	// A consumer that drains stdout to EOF before calling Wait must not
	// deadlock: the exit status is parked and the pipes close without a
	// waiting receiver.
	stream1 := newFakeRunStream(t.Context(), nil)

	rc := NewRemoteRunningCommand(stream1)

	stream1.recvCh <- stdoutResp("hi")

	stream1.recvCh <- exitResp(3)

	out, err := io.ReadAll(rc.Stdout())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(out), test.ShouldEqual, "hi")

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 3)
}

func TestRemoteRunningCommandNonResumableStillErrors(t *testing.T) {
	stream1 := newFakeRunStream(t.Context(), nil)

	rc := NewRemoteRunningCommand(stream1)

	stream1.recvCh <- fakeRecvItem{err: io.ErrUnexpectedEOF}

	_, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldNotBeNil)
}

func TestRemoteRunningCommandStdinEOFReplayedAfterResume(t *testing.T) {
	// The consumer half-closed stdin while the transport was down; the fresh
	// stream must be half-closed too or a piped command waiting on EOF would
	// hang forever.
	stream1 := newFakeRunStream(t.Context(), func(*graftv1.RunCommandRequest) error {
		return status.Error(codes.Unavailable, "transport broke")
	})
	stream2 := newFakeRunStream(t.Context(), nil)

	reattach := func(_ context.Context, stdoutOffset, stderrOffset uint64) (
		runCommandStreamClient, *graftv1.CommandAttached, error,
	) {
		return stream2, &graftv1.CommandAttached{
			StdoutReplayOffset: stdoutOffset,
			StderrReplayOffset: stderrOffset,
			Running:            true,
		}, nil
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	// Consumer signals end of input while stream1 is broken.
	test.That(t, rc.Stdin().Close(), test.ShouldBeNil)

	select {
	case <-stream1.closeSends:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the initial half-close")
	}

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Unavailable, "transport broke")}

	select {
	case <-stream2.closeSends:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the half-close to be replayed on the fresh stream")
	}

	stream2.recvCh <- exitResp(0)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
}

func TestRemoteRunningCommandConsumerGoneUnblocksPipeWrites(t *testing.T) {
	// A consumer that vanishes mid-output must not leave process() wedged in
	// a pipe write nobody will ever read.
	streamCtx, cancel := context.WithCancel(t.Context())

	stream1 := newFakeRunStream(streamCtx, nil)

	reattach := func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
		return nil, nil, status.Error(codes.NotFound, "gone")
	}

	rc := newResumableRemoteRunningCommand(streamCtx, stream1, "cmd1", "conn1", reattach, 0, 0, nil)

	// Output arrives but nobody ever reads rc.Stdout(): process() blocks in
	// the pipe write.
	stream1.recvCh <- stdoutResp("wedge")

	// The consumer goes away entirely.
	cancel()

	_, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldNotBeNil)
}

func TestRemoteRunningCommandDeliberateDetachHook(t *testing.T) {
	streamCtx, cancel := context.WithCancel(t.Context())

	stream1 := newFakeRunStream(streamCtx, nil)

	reattach := func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
		return nil, nil, status.Error(codes.NotFound, "gone")
	}

	detached := make(chan struct{})
	rc := newResumableRemoteRunningCommand(streamCtx, stream1, "cmd1", "conn1", reattach, 0, 0,
		func() { close(detached) })

	// The consumer leaves on purpose: the hook must fire so the daemon can
	// clean up a kill-policy command immediately.
	cancel()

	select {
	case <-detached:
	case <-time.After(10 * time.Second):
		t.Fatal("deliberate detach hook did not fire on context cancel")
	}

	stream1.recvCh <- fakeRecvItem{err: status.Error(codes.Canceled, "context canceled")}

	_, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldNotBeNil)
}

func TestRemoteRunningCommandDeliberateDetachHookNotFiredOnExit(t *testing.T) {
	stream1 := newFakeRunStream(t.Context(), nil)

	hookFired := make(chan struct{}, 1)
	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1",
		func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
			return nil, nil, status.Error(codes.NotFound, "gone")
		}, 0, 0,
		func() { hookFired <- struct{}{} })

	stream1.recvCh <- exitResp(0)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)

	select {
	case <-hookFired:
		t.Fatal("hook fired for a normally exited command")
	default:
	}
}

func TestRemoteRunningCommandSendsConsumptionAcks(t *testing.T) {
	acks := make(chan *graftv1.CommandAck, 8)
	stream1 := newFakeRunStream(t.Context(), func(req *graftv1.RunCommandRequest) error {
		if data, ok := req.GetData().(*graftv1.RunCommandRequest_Ack); ok {
			acks <- data.Ack
		}

		return nil
	})

	reattach := func(context.Context, uint64, uint64) (runCommandStreamClient, *graftv1.CommandAttached, error) {
		return nil, nil, status.Error(codes.NotFound, "gone")
	}

	rc := newResumableRemoteRunningCommand(t.Context(), stream1, "cmd1", "conn1", reattach, 0, 0, nil)
	stdout := collectStdout(rc)

	// Deliver just over the ack threshold; consumption must be confirmed back
	// so the daemon can release buffer space for lossless commands.
	chunk := strings.Repeat("x", 64<<10)
	for range 3 {
		stream1.recvCh <- stdoutResp(chunk)
	}

	select {
	case ack := <-acks:
		test.That(t, ack.GetStdoutOffset(), test.ShouldBeGreaterThanOrEqualTo, uint64(commandAckEveryBytes))
	case <-time.After(10 * time.Second):
		t.Fatal("no consumption ack was sent")
	}

	stream1.recvCh <- exitResp(0)

	waitStatus, waitErr := rc.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
	test.That(t, len(stdout()), test.ShouldEqual, 3*(64<<10))
}

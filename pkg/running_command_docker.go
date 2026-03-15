package graft

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/edaniels/graft/errors"
)

// DockerRunningCommand encapsulates a [RunningCommand] for a docker based connection. It should only be
// used for internal setup and all other commands should go through the UDS via RemoteRunningCommand.
type DockerRunningCommand struct {
	hijackedConn  client.HijackedResponse
	inspect       func() (client.ExecInspectResult, error)
	resizeWindow  func(opts client.ContainerResizeOptions) error
	kill          func(sig, pid string) (string, error)
	disableSignal bool

	stdin       io.WriteCloser
	stdoutPipeR io.ReadCloser
	stdoutPipeW io.WriteCloser
	stderrPipeR io.ReadCloser
	stderrPipeW io.WriteCloser

	waited     atomic.Bool
	processing sync.WaitGroup

	pidMu sync.Mutex
	pid   string
}

// NewDockerRunningCommand returns and starts processing a running command originating from this host's docker daemon.
// In addition to the connection to the docker exec process, the command also needs a way to self-inspect the process,
// resize it, and kill it, because this is required by the [RunningCommand] interface.
//
// See [dockerConnection.runContainerExec] for creating the [types.HijackedResponse].
func NewDockerRunningCommand(
	hijackedConn client.HijackedResponse,
	inspect func() (client.ExecInspectResult, error),
	resizeWindow func(opts client.ContainerResizeOptions) error,
	kill func(sig, pid string) (string, error),
	stdin io.WriteCloser,
	// disableSignal is a stupid hack that prevents us from recursively making new RunningCommands
	// because signaling for docker commands right now has us run another... RunningCommand.
	disableSignal bool,
) (*DockerRunningCommand, error) {
	// establish pipes - we have a read/write side for each since this command is forwarding
	// from a client (the on the terminal) and a server (the one running the process)

	// stdout:
	// consumer - reads from read side
	// demuxer - receives from exec process and writes to write side
	// process - sends to demuxers read side
	stdoutPipeR, stdoutPipeW := io.Pipe()
	// stderr:
	// consumer - reads from read side
	// demuxer - receives from exec process and writes to write side
	// process - sends to demuxers read side
	stderrPipeR, stderrPipeW := io.Pipe()

	cmd := &DockerRunningCommand{
		hijackedConn:  hijackedConn,
		inspect:       inspect,
		resizeWindow:  resizeWindow,
		disableSignal: disableSignal,
		kill:          kill,

		stdin:       stdin,
		stdoutPipeR: stdoutPipeR,
		stdoutPipeW: stdoutPipeW,
		stderrPipeR: stderrPipeR,
		stderrPipeW: stderrPipeW,
	}
	cmd.processing.Go(cmd.process)

	var (
		pidStrBytes   [1]byte
		pidStrBuilder strings.Builder
	)

	for {
		n, err := stdoutPipeR.Read(pidStrBytes[:])
		if err != nil {
			_, stopErr := StopCommand(cmd)

			return nil, errors.Join(err, stopErr)
		}

		if n < 1 {
			_, stopErr := StopCommand(cmd)

			return nil, errors.Join(errors.New("expected a byte"), stopErr)
		}

		ch := pidStrBytes[0]
		// TODO(erd): Verify PID output contains only the PID string and newline.
		if ch == '\n' {
			break
		}

		pidStrBuilder.WriteString(string(ch))
	}

	cmd.pidMu.Lock()
	cmd.pid = pidStrBuilder.String()
	cmd.pidMu.Unlock()

	return cmd, nil
}

func (rc *DockerRunningCommand) Stdin() io.WriteCloser {
	return rc.stdin
}

func (rc *DockerRunningCommand) Stdout() io.Reader {
	return rc.stdoutPipeR
}

func (rc *DockerRunningCommand) Stderr() io.Reader {
	return rc.stderrPipeR
}

// Signal is a bit more involved than other [RunningCommand]s, because right now
// it does a self inspection as well a request to the docker daemon to send kill
// signals. Ideally it can be simplified.
func (rc *DockerRunningCommand) Signal(sig string) error {
	if rc.disableSignal {
		return errors.New("signal disabled")
	}

	rc.pidMu.Lock()
	pid := rc.pid
	rc.pidMu.Unlock()

	if pid == "" {
		return nil
	}

	sig = strings.TrimPrefix(sig, "SIG")

	killRet, err := rc.kill(sig, pid)
	if err != nil {
		execInspect, inspectErr := rc.inspect()
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}

		if !execInspect.Running {
			return nil
		}

		return errors.WrapPrefix(err, killRet)
	}

	return nil
}

// Wait spins until the underlying docker exec process is finished, cleans up any
// resources it created after waiting, and returns either the exit status or an unexpected error.
func (rc *DockerRunningCommand) Wait() (int, error) {
	defer rc.processing.Wait()

	defer func() {
		rc.waited.Store(true)
		rc.hijackedConn.Conn.Close()

		// Close the pipe readers so that any StdCopy write blocked on an
		// io.Pipe (because no goroutine is reading from the pipe) fails
		// with ErrClosedPipe and the process() goroutine can exit.
		rc.stdoutPipeR.Close()
		rc.stderrPipeR.Close()
	}()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		execInspect, err := rc.inspect()
		if err != nil {
			return -1, errors.Wrap(err)
		}

		if execInspect.Running {
			continue
		}

		return execInspect.ExitCode, nil
	}

	return -1, errors.New("unreachable")
}

// Release does nothing to cleanup resources since it is handled in process.
func (rc *DockerRunningCommand) Release() {
}

func (rc *DockerRunningCommand) SetEnvVar(_, _ string) error {
	return errors.New("cannot set env var after start")
}

// NotifyWindowChange instructs the docker runtime to tell the underlying exec process that the window
// size has changed.
func (rc *DockerRunningCommand) NotifyWindowChange(h, w int) error {
	return rc.resizeWindow(client.ContainerResizeOptions{
		Height: uint(h), //nolint:gosec // overflow okay
		Width:  uint(w), //nolint:gosec // overflow okay
	})
}

// process demuxes stdout/err into its corresponding pipes. Besides this, a docker command is quite close to
// a local command.
func (rc *DockerRunningCommand) process() {
	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Go(func() {
		// These closes are critical to unblocking readers/writers once hijacked.Reader (containing stdout/err)
		// is closed/EOFd.
		defer rc.stdoutPipeW.Close()
		defer rc.stderrPipeW.Close()

		// Note(erd): The way this works makes using io.Pipes a pain in the ass since we need to read from
		// both, otherwise we block on one of the reads/writes based on how io.Pipe works. We end up just
		// collecting data in all these cases.
		if _, copyErr := stdcopy.StdCopy(rc.stdoutPipeW, rc.stderrPipeW, rc.hijackedConn.Reader); copyErr != nil {
			if rc.waited.Load() {
				return
			}

			slog.DebugContext(context.Background(), "error demuxing command", "error", copyErr)
		}
	})
}

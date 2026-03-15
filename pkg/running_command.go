package graft

import (
	"io"

	"github.com/edaniels/graft/errors"
)

// A RunningCommand is the interface for consuming a command as a client. This direction is important
// for understanding if you should be reading/writing to files.
type RunningCommand interface {
	// Stdin returns the write input to the command. The consumer is responsible for closing stdin when done with it.
	Stdin() io.WriteCloser

	// Stdout is the standard output stream.
	Stdout() io.Reader

	// Stderr is the standard error stream. It may result in EOF to begin with if this is a simple interactive session;
	// Otherwise, for redirected stream cases, this is probably set to a real reader.
	Stderr() io.Reader

	// Signal sends a signal to the process.
	// TODO(erd): should sig be enum?
	Signal(sig string) error

	// Wait blocks until the command is finished and returns its exit status.
	Wait() (int, error)

	// Release cleans up most remaining resources without waiting.
	Release()

	// SetEnvVar tells the process to set a single environment variable.
	// TODO(erd): Consider splitting SetEnvVar into a separate interface since not all implementations support it.
	SetEnvVar(key, value string) error

	// NotifyWindowChange informs the underlying pty that the window size of the client has changed. This is
	// critical for applications like top that are rendering their text based on the size of the window.
	NotifyWindowChange(h, w int) error
}

// StopCommand is a helper for terminating a command and waiting for it to stop.
func StopCommand(runningCmd RunningCommand) (int, error) {
	if err := runningCmd.Signal(SignalTerminate); err != nil {
		return -1, errors.WrapPrefix(err, "error sending singal")
	}

	status, err := runningCmd.Wait()
	if err != nil {
		return -1, errors.WrapPrefix(err, "error waiting")
	}

	return status, nil
}

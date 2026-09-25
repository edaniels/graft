package graft

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edaniels/graft/errors"
)

// replaceSIGTERMWait is how long --replace waits for the old daemon to exit
// after SIGTERM before escalating to SIGKILL. The window covers the old
// daemon's worst-case orderly shutdown: terminating its managed commands
// (kill grace + drain grace, both bounded) plus slack. Escalating too early
// would SIGKILL it mid-cleanup and leave that work to the new daemon's
// stale-command reconciliation. The wait polls, so a healthy daemon exits
// well before the deadline. A variable for tests.
var replaceSIGTERMWait = defaultKillGrace + defaultDrainGrace + 5*time.Second

// processSignal is syscall.Kill; a variable so tests can simulate process
// death racing signal delivery.
var processSignal = syscall.Kill

// killDaemonByPIDFile reads a PID file, kills the process identified by it,
// and removes the PID file. It is used during --replace to terminate a
// previous daemon before taking over. A nil error means no old daemon
// survives (it may never have existed); any failure to kill a live process is
// returned so the caller can abort instead of starting a second daemon over a
// live one.
func killDaemonByPIDFile(pidPath string) error {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		// The old daemon can neither be identified nor killed; starting
		// anyway would collide with it at the socket bind.
		return errors.WrapPrefix(err, "error reading PID file "+pidPath)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		slog.Warn("invalid PID file content, removing", "path", pidPath, "content", string(data))
		os.Remove(pidPath)

		return nil //nolint:nilerr // unparsable content means no live daemon to kill
	}

	// Check if the process is alive.
	if err := processSignal(pid, 0); err != nil {
		// Process is dead; clean up stale PID file.
		os.Remove(pidPath)

		return nil //nolint:nilerr // the probe error here means the process is gone
	}

	slog.Info("killing previous daemon", "pid", pid)

	// Try graceful shutdown with SIGTERM first.
	if err := processSignal(pid, syscall.SIGTERM); err != nil {
		// The process died between the liveness probe and the signal; ESRCH
		// means it is gone, which is exactly what --replace wants.
		if errors.Is(err, syscall.ESRCH) {
			os.Remove(pidPath)

			return nil
		}

		return errors.WrapPrefix(err, "error sending SIGTERM to old daemon (pid "+strconv.Itoa(pid)+")")
	}

	if waitForProcessDeath(pid, replaceSIGTERMWait) {
		os.Remove(pidPath)

		return nil
	}

	// Still alive after SIGTERM wait; escalate to SIGKILL.
	slog.Warn("old daemon did not exit after SIGTERM, sending SIGKILL", "pid", pid)

	if err := processSignal(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			os.Remove(pidPath)

			return nil
		}

		return errors.WrapPrefix(err, "error sending SIGKILL to old daemon (pid "+strconv.Itoa(pid)+")")
	}

	if !waitForProcessDeath(pid, 2*time.Second) {
		return errors.New("old daemon (pid " + strconv.Itoa(pid) + ") still alive after SIGKILL")
	}

	os.Remove(pidPath)

	return nil
}

// waitForProcessDeath polls kill(pid, 0) until the process is dead or the
// timeout expires. Returns true if the process died.
func waitForProcessDeath(pid int, timeout time.Duration) bool {
	const pollInterval = 50 * time.Millisecond

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if err := processSignal(pid, 0); err != nil {
			return true
		}

		time.Sleep(pollInterval)
	}

	return false
}

package graft

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killDaemonByPIDFile is a best-effort helper that reads a PID file, kills the
// process identified by it, and removes the PID file. It is used during
// --replace to terminate a previous daemon before taking over.
func killDaemonByPIDFile(pidPath string) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}

		slog.Warn("error reading PID file", "path", pidPath, "error", err)

		return
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		slog.Warn("invalid PID file content, removing", "path", pidPath, "content", string(data))
		os.Remove(pidPath)

		return
	}

	// Check if the process is alive.
	if err := syscall.Kill(pid, 0); err != nil {
		// Process is dead; clean up stale PID file.
		os.Remove(pidPath)

		return
	}

	slog.Info("killing previous daemon", "pid", pid)

	// Try graceful shutdown with SIGTERM first.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		slog.Warn("error sending SIGTERM to old daemon", "pid", pid, "error", err)
		os.Remove(pidPath)

		return
	}

	if waitForProcessDeath(pid, 2*time.Second) {
		os.Remove(pidPath)

		return
	}

	// Still alive after SIGTERM wait - escalate to SIGKILL.
	slog.Warn("old daemon did not exit after SIGTERM, sending SIGKILL", "pid", pid)

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		slog.Warn("error sending SIGKILL to old daemon", "pid", pid, "error", err)
		os.Remove(pidPath)

		return
	}

	waitForProcessDeath(pid, 2*time.Second)
	os.Remove(pidPath)
}

// waitForProcessDeath polls kill(pid, 0) until the process is dead or the
// timeout expires. Returns true if the process died.
func waitForProcessDeath(pid int, timeout time.Duration) bool {
	const pollInterval = 50 * time.Millisecond

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}

		time.Sleep(pollInterval)
	}

	return false
}

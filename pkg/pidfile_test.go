package graft

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.viam.com/test"
)

func TestKillDaemonByPIDFileNoPIDFile(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	// Should be a no-op when the file doesn't exist.
	killDaemonByPIDFile(pidPath)
}

func TestKillDaemonByPIDFileInvalidContent(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	test.That(t, os.WriteFile(pidPath, []byte("garbage"), 0o600), test.ShouldBeNil)

	killDaemonByPIDFile(pidPath)

	// File should be removed after invalid content.
	_, err := os.Stat(pidPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestKillDaemonByPIDFileStalePID(t *testing.T) {
	// Start a process and wait for it to exit so we have a dead PID.
	cmd := exec.CommandContext(context.Background(), "true")
	test.That(t, cmd.Start(), test.ShouldBeNil)
	test.That(t, cmd.Wait(), test.ShouldBeNil)

	deadPID := cmd.Process.Pid

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)), 0o600), test.ShouldBeNil)

	killDaemonByPIDFile(pidPath)

	// File should be cleaned up for a dead process.
	_, err := os.Stat(pidPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestKillDaemonByPIDFileLiveProcess(t *testing.T) {
	// Start a sleep process that we can kill.
	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	test.That(t, cmd.Start(), test.ShouldBeNil)

	pid := cmd.Process.Pid

	// Reap in background so zombie doesn't linger after kill.
	waitDone := make(chan struct{})

	go func() {
		cmd.Wait() //nolint:errcheck
		close(waitDone)
	}()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600), test.ShouldBeNil)

	killDaemonByPIDFile(pidPath)

	// Wait for reaping to complete.
	<-waitDone

	// Process should be dead.
	err := syscall.Kill(pid, 0)
	test.That(t, err, test.ShouldNotBeNil)

	// PID file should be removed.
	_, err = os.Stat(pidPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestKillDaemonByPIDFileSIGTERMResistant(t *testing.T) {
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")

	// Start a bash process that traps SIGTERM, signals readiness, then sleeps.
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "trap '' TERM; touch "+readyPath+"; sleep 60") //nolint:gosec // test helper
	test.That(t, cmd.Start(), test.ShouldBeNil)

	pid := cmd.Process.Pid

	// Reap in background so zombie doesn't linger after kill.
	waitDone := make(chan struct{})

	go func() {
		cmd.Wait() //nolint:errcheck
		close(waitDone)
	}()

	// Wait for the trap to be set up.
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	pidPath := filepath.Join(dir, "graftd.pid")

	test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600), test.ShouldBeNil)

	done := make(chan struct{})

	go func() {
		defer close(done)

		killDaemonByPIDFile(pidPath)
	}()

	// Should complete within a reasonable time (SIGTERM wait + SIGKILL wait + buffer).
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("killDaemonByPIDFile did not complete in time")
	}

	// Wait for reaping to complete.
	<-waitDone

	// Process should be dead after SIGKILL.
	err := syscall.Kill(pid, 0)
	test.That(t, err, test.ShouldNotBeNil)

	// PID file should be removed.
	_, err = os.Stat(pidPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

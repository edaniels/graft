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
	test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)
}

func TestKillDaemonByPIDFileUnreadable(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	// A directory where the PID file should be cannot be read; the old daemon
	// is then unidentifiable and must be treated as unkillable so the caller
	// can abort instead of starting a second daemon over it.
	test.That(t, os.Mkdir(pidPath, 0o700), test.ShouldBeNil)

	err := killDaemonByPIDFile(pidPath)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "error reading PID file")
}

func TestKillDaemonByPIDFileInvalidContent(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "graftd.pid")

	test.That(t, os.WriteFile(pidPath, []byte("garbage"), 0o600), test.ShouldBeNil)

	test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)

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

	test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)

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

	test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)

	// Wait for reaping to complete.
	<-waitDone

	// Process should be dead.
	err := syscall.Kill(pid, 0)
	test.That(t, err, test.ShouldNotBeNil)

	// PID file should be removed.
	_, err = os.Stat(pidPath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestKillDaemonByPIDFileESRCHRaces(t *testing.T) {
	t.Run("daemon dies between liveness probe and SIGTERM", func(t *testing.T) {
		old := processSignal

		t.Cleanup(func() { processSignal = old })

		processSignal = func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				// Process already gone: SIGTERM races its exit.
				return syscall.ESRCH
			}

			return syscall.Kill(pid, sig)
		}

		// A live process so the initial liveness probe succeeds.
		cmd := exec.CommandContext(context.Background(), "sleep", "60")
		test.That(t, cmd.Start(), test.ShouldBeNil)

		waitDone := make(chan struct{})

		go func() {
			cmd.Wait() //nolint:errcheck
			close(waitDone)
		}()

		pid := cmd.Process.Pid

		t.Cleanup(func() {
			cmd.Process.Kill() //nolint:errcheck // best effort cleanup
			<-waitDone
		})

		dir := t.TempDir()
		pidPath := filepath.Join(dir, "graftd.pid")
		test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600), test.ShouldBeNil)

		// ESRCH means the old daemon is gone, which is exactly what --replace
		// wants; it must not abort.
		test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)

		_, err := os.Stat(pidPath)
		test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	})

	t.Run("daemon dies between SIGTERM wait and SIGKILL", func(t *testing.T) {
		old := processSignal

		t.Cleanup(func() { processSignal = old })

		oldWait := replaceSIGTERMWait
		replaceSIGTERMWait = 100 * time.Millisecond

		t.Cleanup(func() { replaceSIGTERMWait = oldWait })

		processSignal = func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				return syscall.ESRCH
			}

			return syscall.Kill(pid, sig)
		}

		// SIGTERM-resistant (trapped) but SIGKILLable live process.
		dir := t.TempDir()
		readyPath := filepath.Join(dir, "ready")

		trapCmd := "trap '' TERM; touch " + readyPath + "; sleep 60"
		cmd := exec.CommandContext(context.Background(), "bash", "-c", trapCmd)
		test.That(t, cmd.Start(), test.ShouldBeNil)

		waitDone := make(chan struct{})

		go func() {
			cmd.Wait() //nolint:errcheck
			close(waitDone)
		}()

		pid := cmd.Process.Pid

		t.Cleanup(func() {
			cmd.Process.Kill() //nolint:errcheck // best effort cleanup
			<-waitDone
		})

		deadline := time.Now().Add(5 * time.Second)

		for {
			if _, err := os.Stat(readyPath); err == nil {
				break
			}

			if time.Now().After(deadline) {
				test.That(t, "trapped process never signaled readiness", test.ShouldBeEmpty)
			}

			time.Sleep(10 * time.Millisecond)
		}

		pidPath := filepath.Join(dir, "graftd.pid")
		test.That(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600), test.ShouldBeNil)

		test.That(t, killDaemonByPIDFile(pidPath), test.ShouldBeNil)

		_, err := os.Stat(pidPath)
		test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	})
}

func TestKillDaemonByPIDFileSIGTERMResistant(t *testing.T) {
	// The production SIGTERM window is generous (it covers a daemon
	// gracefully terminating its managed commands); shorten it so the test
	// exercises the SIGKILL escalation quickly.
	oldWait := replaceSIGTERMWait
	replaceSIGTERMWait = 500 * time.Millisecond

	t.Cleanup(func() { replaceSIGTERMWait = oldWait })

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

	killErr := make(chan error, 1)

	go func() { killErr <- killDaemonByPIDFile(pidPath) }()

	// Should complete within a reasonable time (SIGTERM wait + SIGKILL wait + buffer).
	select {
	case err := <-killErr:
		test.That(t, err, test.ShouldBeNil)
	case <-time.After(10 * time.Second):
		test.That(t, "killDaemonByPIDFile did not complete in time", test.ShouldBeEmpty)
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

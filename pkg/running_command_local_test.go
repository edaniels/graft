package graft

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.viam.com/test"
)

// waitForProcessGone polls until the pid no longer exists, or fails the test
// after a deadline.
func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	deadline := time.After(5 * time.Second)

	for {
		if killErr := syscall.Kill(pid, syscall.Signal(0)); killErr != nil {
			return
		}

		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("process %d still alive", pid)
		}
	}
}

func TestExecuteLocalCommandNonPtyGetsOwnProcessGroup(t *testing.T) {
	cmd, err := ExecuteLocalCommand(
		t.Context(),
		[]string{"sh", "-c", "read line"},
		false, // no pty
		false, // no redirect stdout
		false, // no redirect stderr
	)
	test.That(t, err, test.ShouldBeNil)

	pgid, err := syscall.Getpgid(cmd.PID())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pgid, test.ShouldEqual, cmd.PID())
	test.That(t, pgid, test.ShouldNotEqual, syscall.Getpgrp())

	test.That(t, cmd.Stdin().Close(), test.ShouldBeNil)

	_, waitErr := cmd.Wait()
	cmd.Release()
	test.That(t, waitErr, test.ShouldBeNil)
}

func TestLocalCommandSignalReachesGrandchildren(t *testing.T) {
	// The shell prints the pid of a backgrounded sleep (a grandchild of the
	// daemon) and then waits. Signaling the command must terminate the whole
	// process group, grandchild included.
	cmd, err := ExecuteLocalCommand(
		t.Context(),
		[]string{"sh", "-c", "sleep 300 & echo $!; wait"},
		false, // no pty
		false, // no redirect stdout
		false, // no redirect stderr
	)
	test.That(t, err, test.ShouldBeNil)

	line, err := bufio.NewReader(cmd.Stdout()).ReadString('\n')
	test.That(t, err, test.ShouldBeNil)

	grandchildPID, err := strconv.Atoi(strings.TrimSpace(line))
	test.That(t, err, test.ShouldBeNil)

	test.That(t, cmd.Signal(SignalTerminate), test.ShouldBeNil)

	_, waitErr := cmd.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	cmd.Release()

	waitForProcessGone(t, grandchildPID)
	waitForProcessGone(t, cmd.PID())
}

func TestLocalCommandSignalKill(t *testing.T) {
	cmd, err := ExecuteLocalCommand(
		t.Context(),
		[]string{"sh", "-c", "trap '' TERM; read line"},
		false, // no pty
		false, // no redirect stdout
		false, // no redirect stderr
	)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, cmd.Signal(SignalKill), test.ShouldBeNil)

	_, waitErr := cmd.Wait()
	test.That(t, waitErr, test.ShouldBeNil)
	cmd.Release()

	waitForProcessGone(t, cmd.PID())
}

func TestExecuteLocalCommandRedirectStdoutCompletes(t *testing.T) {
	// This test verifies that a command with redirected stdout
	// properly delivers output and exits without hanging. Previously,
	// io.Pipe was used for the redirect which caused a deadlock:
	// the pipe writer was only closed in Release() which ran after
	// waiting for readers, but readers blocked waiting for EOF from
	// the never-closed pipe writer.
	cmd, err := ExecuteLocalCommand(
		t.Context(),
		[]string{"echo", "hello"},
		false, // no pty
		true,  // redirect stdout
		false, // no redirect stderr
	)
	test.That(t, err, test.ShouldBeNil)

	done := make(chan struct{})

	var (
		stdout  []byte
		readErr error
	)

	go func() {
		defer close(done)

		stdout, readErr = io.ReadAll(cmd.Stdout())
	}()

	waitStatus, waitErr := cmd.Wait()
	cmd.Release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stdout reader to finish")
	}

	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
	test.That(t, string(stdout), test.ShouldEqual, "hello\n")
}

func TestExecuteLocalCommandRedirectStderrCompletes(t *testing.T) {
	cmd, err := ExecuteLocalCommand(
		t.Context(),
		[]string{"sh", "-c", "echo err >&2"},
		false, // no pty
		false, // no redirect stdout
		true,  // redirect stderr
	)
	test.That(t, err, test.ShouldBeNil)

	done := make(chan struct{})

	var (
		stderr  []byte
		readErr error
	)

	go func() {
		defer close(done)

		stderr, readErr = io.ReadAll(cmd.Stderr())
	}()

	waitStatus, waitErr := cmd.Wait()
	cmd.Release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stderr reader to finish")
	}

	test.That(t, readErr, test.ShouldBeNil)
	test.That(t, waitErr, test.ShouldBeNil)
	test.That(t, waitStatus, test.ShouldEqual, 0)
	test.That(t, string(stderr), test.ShouldEqual, "err\n")
}

func TestSignalFromNameAliases(t *testing.T) {
	for _, name := range []string{"SIGTERM", "TERM", "term", "sigterm"} {
		sig, err := signalFromName(name)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, sig, test.ShouldEqual, syscall.SIGTERM)
	}

	sig, err := signalFromName("usr1")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, sig, test.ShouldEqual, syscall.SIGUSR1)

	_, err = signalFromName("NOTASIGNAL")
	test.That(t, err, test.ShouldNotBeNil)
}

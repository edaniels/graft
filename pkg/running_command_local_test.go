package graft

import (
	"io"
	"testing"
	"time"

	"go.viam.com/test"
)

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

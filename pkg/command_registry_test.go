package graft

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.viam.com/test"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

func startManagedCommand(t *testing.T, reg *CommandRegistry, command []string, spec ManagedCommandSpec) *ManagedCommand {
	t.Helper()

	cmd, err := ExecuteLocalCommand(t.Context(), command, false, false, false)
	test.That(t, err, test.ShouldBeNil)

	managed, err := reg.Register(cmd, spec)
	test.That(t, err, test.ShouldBeNil)

	return managed
}

func waitDone(t *testing.T, managed *ManagedCommand) {
	t.Helper()

	select {
	case <-managed.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for command to finish")
	}
}

func TestCommandRegistryRegisterAndList(t *testing.T) {
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line"}, ManagedCommandSpec{
		Display:     "read line",
		CWD:         "/tmp",
		Persistence: persistenceKeep,
	})

	test.That(t, managed.ID(), test.ShouldNotBeEmpty)

	infos := reg.List()
	test.That(t, len(infos), test.ShouldEqual, 1)
	test.That(t, infos[0].GetCommandId(), test.ShouldEqual, managed.ID())
	test.That(t, infos[0].GetCommand(), test.ShouldEqual, "read line")
	test.That(t, infos[0].GetCwd(), test.ShouldEqual, "/tmp")
	test.That(t, infos[0].GetRunning(), test.ShouldBeTrue)
	test.That(t, infos[0].GetAttached(), test.ShouldBeFalse)

	got, ok := reg.Get(managed.ID())
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, got, test.ShouldEqual, managed)

	test.That(t, managed.Signal(SignalTerminate), test.ShouldBeNil)
	waitDone(t, managed)

	infos = reg.List()
	test.That(t, len(infos), test.ShouldEqual, 1)
	test.That(t, infos[0].GetRunning(), test.ShouldBeFalse)

	reg.Remove(managed.ID())
	test.That(t, reg.List(), test.ShouldBeEmpty)
}

func TestCommandRegistryDrainsWithoutReader(t *testing.T) {
	// A command that produces far more output than the OS pipe buffer must be
	// able to run to completion with no client attached; the registry's
	// drainers keep consuming into the ring so the process never wedges.
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "yes 0123456789abcdef | head -c 300000; printf done"},
		ManagedCommandSpec{Display: "spam", Persistence: persistenceKeep, StdoutCapacity: 1024},
	)

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 0)

	// The ring retained only the newest bytes; the tail must be present.
	data, _, err := managed.outRing().ReadAt(managed.outRing().StartOffset(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.HasSuffix(string(data), "done"), test.ShouldBeTrue)
	test.That(t, managed.outRing().EndOffset(), test.ShouldEqual, 300004)
}

func TestCommandRegistryAttachReplayAfterExit(t *testing.T) {
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg, []string{"echo", "hello"}, ManagedCommandSpec{
		Display:     "echo hello",
		Persistence: persistenceKeep,
	})

	waitDone(t, managed)

	handle := managed.attach()
	defer managed.ClientGone(handle)

	data, gotOffset, err := managed.outRing().ReadAt(0, handle.Canceled())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, gotOffset, test.ShouldEqual, 0)
	test.That(t, string(data), test.ShouldEqual, "hello\n")

	_, _, err = managed.outRing().ReadAt(gotOffset+uint64(len(data)), handle.Canceled())
	test.That(t, err, test.ShouldEqual, io.EOF)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 0)
}

func TestCommandRegistryKillPolicyOnClientGone(t *testing.T) {
	reg := NewCommandRegistry("")
	reg.killGrace = 100 * time.Millisecond
	reg.detachKillDelay = 50 * time.Millisecond

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "trap '' TERM; sleep 300"},
		ManagedCommandSpec{Display: "stubborn", Persistence: persistenceKill},
	)

	handle := managed.attach()
	managed.ClientGone(handle)

	// Even a TERM-ignoring command must die via KILL escalation.
	waitDone(t, managed)
	waitForProcessGone(t, managed.PID())
}

func TestCommandRegistryReattachCancelsPendingKill(t *testing.T) {
	// A transient transport break looks like a disconnect; a client that
	// re-attaches within the delay must call off the kill.
	reg := NewCommandRegistry("")
	reg.killGrace = 100 * time.Millisecond
	reg.detachKillDelay = 100 * time.Millisecond

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line; exit 7"}, ManagedCommandSpec{
		Display:     "read line",
		Persistence: persistenceKill,
	})

	first := managed.attach()
	managed.ClientGone(first)

	second := managed.attach()
	defer managed.ClientGone(second)

	// Well past the would-be kill: the command must still be running.
	select {
	case <-managed.Done():
		t.Fatal("command was killed despite the re-attach")
	case <-time.After(400 * time.Millisecond):
	}

	// And functional: stdin still drives it to its own exit status.
	_, err := managed.Stdin().Write([]byte("x\n"))
	test.That(t, err, test.ShouldBeNil)

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 7)
}

func TestCommandRegistryDrainForcedWhenDescendantHoldsOutput(t *testing.T) {
	// The shell exits immediately but a background sleep inherits its stdout
	// pipe. The exit must still be recorded after the drain grace instead of
	// the command staying "running" until the descendant dies.
	reg := NewCommandRegistry("")
	reg.drainGrace = 100 * time.Millisecond

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "sleep 300 & echo started; exit 5"},
		ManagedCommandSpec{Display: "escapee", Persistence: persistenceKeep},
	)

	t.Cleanup(func() {
		// The registry deliberately leaves exited commands' survivors alone;
		// reap the backgrounded sleep here.
		_ = syscall.Kill(-managed.PID(), syscall.SIGKILL) //nolint:errcheck // best-effort test cleanup
	})

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 5)

	// The output produced before the exit was drained.
	data, _, err := managed.outRing().ReadAt(0, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, string(data), test.ShouldEqual, "started\n")
}

func TestCommandRegistryKeepPolicyOnClientGone(t *testing.T) {
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line; exit 7"}, ManagedCommandSpec{
		Display:     "read line",
		Persistence: persistenceKeep,
	})

	handle := managed.attach()
	managed.ClientGone(handle)

	infos := reg.List()
	test.That(t, len(infos), test.ShouldEqual, 1)
	test.That(t, infos[0].GetAttached(), test.ShouldBeFalse)

	// The command must still be alive and functional after the client left:
	// writing stdin lets it exit normally with its own status.
	_, err := managed.Stdin().Write([]byte("x\n"))
	test.That(t, err, test.ShouldBeNil)

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 7)
}

func TestCommandRegistryAttachSteals(t *testing.T) {
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line"}, ManagedCommandSpec{
		Display:     "read line",
		Persistence: persistenceKeep,
	})

	first := managed.attach()
	second := managed.attach()

	select {
	case <-first.Canceled():
	case <-time.After(5 * time.Second):
		t.Fatal("first attachment was not canceled by the second")
	}

	// ClientGone from the stolen handle must not affect the current one.
	managed.ClientGone(first)

	infos := reg.List()
	test.That(t, infos[0].GetAttached(), test.ShouldBeTrue)

	managed.ClientGone(second)

	infos = reg.List()
	test.That(t, infos[0].GetAttached(), test.ShouldBeFalse)

	test.That(t, managed.Signal(SignalKill), test.ShouldBeNil)
	waitDone(t, managed)
}

func TestCommandRegistryStateFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "commands.json")
	reg := NewCommandRegistry(statePath)

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line"}, ManagedCommandSpec{
		Display:     "read line",
		Persistence: persistenceKeep,
	})

	stateRd, err := os.ReadFile(statePath)
	test.That(t, err, test.ShouldBeNil)

	var entries []commandStateEntry

	test.That(t, json.Unmarshal(stateRd, &entries), test.ShouldBeNil)
	test.That(t, len(entries), test.ShouldEqual, 1)
	test.That(t, entries[0].ID, test.ShouldEqual, managed.ID())
	test.That(t, entries[0].PID, test.ShouldEqual, managed.PID())

	test.That(t, managed.Signal(SignalKill), test.ShouldBeNil)
	waitDone(t, managed)
	reg.Remove(managed.ID())

	stateRd, err = os.ReadFile(statePath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, json.Unmarshal(stateRd, &entries), test.ShouldBeNil)
	test.That(t, entries, test.ShouldBeEmpty)
}

func TestCommandRegistryReconcileStaleKillsLeftovers(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "commands.json")

	// Simulate a prior daemon incarnation: a process group leader it spawned
	// is still running and recorded in the state file.
	leftover, err := ExecuteLocalCommand(t.Context(), []string{"sh", "-c", "read line"}, false, false, false)
	test.That(t, err, test.ShouldBeNil)

	entries := []commandStateEntry{{
		ID: "deadbeef", PID: leftover.PID(), StartedAtUnix: time.Now().Unix(), Display: "read line",
	}}
	entriesJSON, err := json.Marshal(entries)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, os.WriteFile(statePath, entriesJSON, 0o600), test.ShouldBeNil)

	// Reap in the background so the process can leave zombie state once the
	// reconcile pass kills it.
	waitCh := make(chan error, 1)

	go func() {
		_, waitErr := leftover.Wait()
		leftover.Release()

		waitCh <- waitErr
	}()

	reg := NewCommandRegistry(statePath)
	reg.killGrace = 100 * time.Millisecond
	reg.ReconcileStale()

	select {
	case waitErr := <-waitCh:
		test.That(t, waitErr, test.ShouldBeNil)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for leftover process to be killed")
	}

	waitForProcessGone(t, leftover.PID())

	// The state file was reset so the next startup doesn't re-reconcile.
	stateRd, err := os.ReadFile(statePath)
	test.That(t, err, test.ShouldBeNil)

	var after []commandStateEntry

	test.That(t, json.Unmarshal(stateRd, &after), test.ShouldBeNil)
	test.That(t, after, test.ShouldBeEmpty)
}

func TestCommandRegistryReconcileStaleSkipsNonGroupLeaders(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "commands.json")

	// A pid that exists but is not a process group leader (this test process's
	// pid unless it happens to lead its group) must not be signaled.
	pid := os.Getpid()
	if pgid, pgErr := syscall.Getpgid(pid); pgErr == nil && pgid == pid {
		t.Skip("test process leads its own group; cannot exercise the guard")
	}

	entries := []commandStateEntry{{ID: "cafef00d", PID: pid, StartedAtUnix: time.Now().Unix(), Display: "self"}}
	entriesJSON, err := json.Marshal(entries)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, os.WriteFile(statePath, entriesJSON, 0o600), test.ShouldBeNil)

	reg := NewCommandRegistry(statePath)
	reg.ReconcileStale()

	// Still alive: signaling ourselves would have failed the test run outright.
	test.That(t, syscall.Kill(pid, syscall.Signal(0)), test.ShouldBeNil)
}

func TestCommandRegistryCloseAll(t *testing.T) {
	reg := NewCommandRegistry("")
	reg.killGrace = 100 * time.Millisecond

	first := startManagedCommand(t, reg, []string{"sh", "-c", "read line"}, ManagedCommandSpec{
		Display: "a", Persistence: persistenceKeep,
	})
	second := startManagedCommand(t, reg, []string{"sh", "-c", "trap '' TERM; sleep 300"}, ManagedCommandSpec{
		Display: "b", Persistence: persistenceKeep,
	})

	reg.CloseAll()

	waitDone(t, first)
	waitDone(t, second)
	waitForProcessGone(t, first.PID())
	waitForProcessGone(t, second.PID())
}

func TestCommandRegistryStdinClosePolicy(t *testing.T) {
	reg := NewCommandRegistry("")

	// Keep commands hold stdin open across a client's half-close so a later
	// attachment can still type.
	kept := startManagedCommand(t, reg, []string{"sh", "-c", "read line; exit 3"}, ManagedCommandSpec{
		Display: "kept", Persistence: persistenceKeep,
	})
	test.That(t, kept.CloseStdinFromClient(), test.ShouldBeNil)

	_, err := kept.Stdin().Write([]byte("x\n"))
	test.That(t, err, test.ShouldBeNil)

	waitDone(t, kept)

	status, _, _ := kept.ExitStatus() //nolint:errcheck // status only
	test.That(t, status, test.ShouldEqual, 3)

	// Kill-policy commands propagate the EOF (piped one-shots rely on it).
	killed := startManagedCommand(t, reg, []string{"cat"}, ManagedCommandSpec{
		Display: "cat", Persistence: persistenceKill,
	})
	test.That(t, killed.CloseStdinFromClient(), test.ShouldBeNil)

	waitDone(t, killed)

	status, _, _ = killed.ExitStatus() //nolint:errcheck // status only
	test.That(t, status, test.ShouldEqual, 0)
}

func TestTailManagedCommandOutputBreaksOnEvictedOffset(t *testing.T) {
	// When a client's expected offset was evicted from the ring, the tail
	// must end the stream (forcing a re-attach with explicit offsets) rather
	// than silently skipping bytes.
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "yes 0123456789abcdef | head -c 4096; printf done"},
		ManagedCommandSpec{Display: "spam", Persistence: persistenceKeep, StdoutCapacity: 512},
	)

	waitDone(t, managed)
	test.That(t, managed.outRing().StartOffset(), test.ShouldBeGreaterThan, 0)

	stop := make(chan struct{})

	var (
		stopOnce  sync.Once
		stopped   bool
		sentBytes int
	)

	closeStop := func() {
		stopOnce.Do(func() {
			stopped = true

			close(stop)
		})
	}

	sendResp := func(resp *graftv1.RunCommandResponse) error {
		sentBytes += len(resp.GetStdout())

		return nil
	}

	tailManagedCommandOutput(managed, sendResp, 0, 0, stop, closeStop)

	test.That(t, stopped, test.ShouldBeTrue)
	test.That(t, sentBytes, test.ShouldEqual, 0)
}

func TestCommandRegistryLosslessKillPolicyBlocksProducer(t *testing.T) {
	// A kill-policy (piped) command producing more than the buffer without
	// anyone confirming consumption must block in write - TCP semantics -
	// rather than losing output.
	reg := NewCommandRegistry("")

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "yes 0123456789abcdef | head -c 200000; printf done"},
		ManagedCommandSpec{Display: "data", Persistence: persistenceKill, StdoutCapacity: 1024},
	)

	// Unconfirmed output fills the ring: the command must NOT be able to run
	// to completion.
	select {
	case <-managed.Done():
		t.Fatal("kill-policy producer completed without consumption; output was lost")
	case <-time.After(300 * time.Millisecond):
	}

	// Consume sequentially with acks: every byte arrives exactly once, in
	// order, and the producer finishes.
	var got []byte

	offset := uint64(0)

	for {
		data, gotOffset, err := managed.outRing().ReadAt(offset, nil)
		if err != nil {
			test.That(t, err, test.ShouldEqual, io.EOF)

			break
		}

		// Lossless: reads never skip ahead.
		test.That(t, gotOffset, test.ShouldEqual, offset)

		got = append(got, data...)
		offset += uint64(len(data))
		managed.AckOutput(offset, 0)
	}

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 0)
	test.That(t, len(got), test.ShouldEqual, 200004)
	test.That(t, strings.HasSuffix(string(got), "done"), test.ShouldBeTrue)
}

func TestCommandRegistryLosslessForcedClosedOnKill(t *testing.T) {
	// A blocked lossless producer must still be killable and reaped: the
	// drain-grace force path closes the rings so nothing wedges.
	reg := NewCommandRegistry("")
	reg.killGrace = 100 * time.Millisecond
	reg.drainGrace = 100 * time.Millisecond

	managed := startManagedCommand(t, reg,
		[]string{"sh", "-c", "yes 0123456789abcdef | head -c 200000; sleep 300"},
		ManagedCommandSpec{Display: "blocked", Persistence: persistenceKill, StdoutCapacity: 1024},
	)

	managed.KillWithEscalation()

	waitDone(t, managed)
	waitForProcessGone(t, managed.PID())
}

func TestCommandRegistryDetachAndKeep(t *testing.T) {
	reg := NewCommandRegistry("")
	reg.detachKillDelay = 100 * time.Millisecond

	managed := startManagedCommand(t, reg, []string{"sh", "-c", "read line; exit 7"}, ManagedCommandSpec{
		Display:     "read line",
		Persistence: persistenceKill,
	})

	handle := managed.attach()

	test.That(t, managed.DetachAndKeep(), test.ShouldBeTrue)

	// The booted attachment observes the takeover...
	select {
	case <-handle.Canceled():
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not cancel the attachment")
	}

	// ...its ClientGone is a stale no-op...
	managed.ClientGone(handle)

	infos := reg.List()
	test.That(t, len(infos), test.ShouldEqual, 1)
	test.That(t, infos[0].GetAttached(), test.ShouldBeFalse)
	test.That(t, infos[0].GetPersistence(), test.ShouldEqual, graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP)

	// ...and the kill-policy delay never fires: the command is alive and
	// functional well past it.
	select {
	case <-managed.Done():
		t.Fatal("detached command was killed")
	case <-time.After(400 * time.Millisecond):
	}

	_, err := managed.Stdin().Write([]byte("x\n"))
	test.That(t, err, test.ShouldBeNil)

	waitDone(t, managed)

	status, exited, exitErr := managed.ExitStatus()
	test.That(t, exitErr, test.ShouldBeNil)
	test.That(t, exited, test.ShouldBeTrue)
	test.That(t, status, test.ShouldEqual, 7)

	// Detaching with nobody attached reports as much.
	test.That(t, managed.DetachAndKeep(), test.ShouldBeFalse)
}

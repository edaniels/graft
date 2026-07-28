package graft

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.viam.com/test"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// startCommandOnDaemon opens a RunCommand stream on its own cancelable
// context and starts the given shell script, returning the stream, the
// started command id, and a cancel func simulating abrupt client death.
func startCommandOnDaemon(
	t *testing.T,
	client graftv1.GraftServiceClient,
	script string,
	persistence graftv1.CommandPersistence,
) (graftv1.GraftService_RunCommandClient, string, context.CancelFunc) {
	t.Helper()

	streamCtx, cancel := context.WithCancel(t.Context())

	runClient, err := client.RunCommand(streamCtx)
	test.That(t, err, test.ShouldBeNil)

	err = runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Start{
			Start: &graftv1.StartCommand{
				Command:     "sh",
				Arguments:   []string{"-c", script},
				Persistence: persistence,
			},
		},
	})
	test.That(t, err, test.ShouldBeNil)

	resp, err := runClient.Recv()
	test.That(t, err, test.ShouldBeNil)

	started, ok := resp.GetData().(*graftv1.RunCommandResponse_Started)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, started.Started.GetCommandId(), test.ShouldNotBeEmpty)

	return runClient, started.Started.GetCommandId(), cancel
}

// recvUntilStdoutContains accumulates stdout from the stream until it
// contains want.
func recvUntilStdoutContains(t *testing.T, runClient graftv1.GraftService_RunCommandClient, want string) {
	t.Helper()

	var out strings.Builder

	for !strings.Contains(out.String(), want) {
		resp, err := runClient.Recv()
		test.That(t, err, test.ShouldBeNil)

		if data, ok := resp.GetData().(*graftv1.RunCommandResponse_Stdout); ok {
			out.Write(data.Stdout)
		}
	}
}

// listCommandsOnDaemon fetches the daemon's managed commands.
func listCommandsOnDaemon(t *testing.T, client graftv1.GraftServiceClient) []*graftv1.CommandInfo {
	t.Helper()

	resp, err := client.ListCommands(t.Context(), &graftv1.ListCommandsRequest{})
	test.That(t, err, test.ShouldBeNil)

	return resp.GetCommands()
}

// waitForCommandState polls the daemon's command list until check passes.
func waitForCommandState(t *testing.T, client graftv1.GraftServiceClient, check func([]*graftv1.CommandInfo) bool) {
	t.Helper()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	deadline := time.After(10 * time.Second)

	for {
		if check(listCommandsOnDaemon(t, client)) {
			return
		}

		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for command state; have: %v", listCommandsOnDaemon(t, client))
		}
	}
}

func TestRunCommandKeepPolicySurvivesDetachAndReattaches(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	client := graftv1.NewGraftServiceClient(connectToTestDaemon(t, sockPath).ClientConn())

	runClient, commandID, cancel := startCommandOnDaemon(t, client,
		"echo one && read line && echo two && exit 5",
		graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP,
	)

	recvUntilStdoutContains(t, runClient, "one\n")

	// Abrupt client death: the command must keep running, detached.
	cancel()

	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 1 && infos[0].GetRunning() && !infos[0].GetAttached() &&
			infos[0].GetCommandId() == commandID
	})

	// Re-attach from offset zero: buffered output replays, stdin works, and
	// the exit status is delivered.
	attachClient, err := client.RunCommand(t.Context())
	test.That(t, err, test.ShouldBeNil)

	err = attachClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Attach{
			Attach: &graftv1.AttachCommand{CommandId: commandID},
		},
	})
	test.That(t, err, test.ShouldBeNil)

	resp, err := attachClient.Recv()
	test.That(t, err, test.ShouldBeNil)

	attached, ok := resp.GetData().(*graftv1.RunCommandResponse_Attached)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, attached.Attached.GetRunning(), test.ShouldBeTrue)
	test.That(t, attached.Attached.GetStdoutReplayOffset(), test.ShouldEqual, 0)

	recvUntilStdoutContains(t, attachClient, "one\n")

	err = attachClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Stdin{Stdin: []byte("x\n")},
	})
	test.That(t, err, test.ShouldBeNil)

	recvUntilStdoutContains(t, attachClient, "two\n")

	for {
		resp, recvErr := attachClient.Recv()
		test.That(t, recvErr, test.ShouldBeNil)

		if data, ok := resp.GetData().(*graftv1.RunCommandResponse_ExitStatus); ok {
			test.That(t, data.ExitStatus, test.ShouldEqual, 5)

			break
		}
	}

	// Delivered exit status removes the command from the list.
	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 0
	})
}

func TestRunCommandKillPolicyDiesOnDetach(t *testing.T) {
	sockPath, srv := startTestRemoteDaemonWithServer(t)
	srv.cmdRegistry.detachKillDelay = 100 * time.Millisecond
	client := graftv1.NewGraftServiceClient(connectToTestDaemon(t, sockPath).ClientConn())

	runClient, _, cancel := startCommandOnDaemon(t, client,
		"echo ready && read line",
		graftv1.CommandPersistence_COMMAND_PERSISTENCE_KILL,
	)

	recvUntilStdoutContains(t, runClient, "ready\n")

	cancel()

	// The kill policy terminates the command and forgets it.
	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 0
	})
}

func TestRunCommandAttachNotFound(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	client := graftv1.NewGraftServiceClient(connectToTestDaemon(t, sockPath).ClientConn())

	attachClient, err := client.RunCommand(t.Context())
	test.That(t, err, test.ShouldBeNil)

	err = attachClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Attach{
			Attach: &graftv1.AttachCommand{CommandId: "nope"},
		},
	})
	test.That(t, err, test.ShouldBeNil)

	_, err = attachClient.Recv()
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, status.Code(err), test.ShouldEqual, codes.NotFound)
}

func TestKillCommandRPC(t *testing.T) {
	sockPath := startTestRemoteDaemon(t)
	client := graftv1.NewGraftServiceClient(connectToTestDaemon(t, sockPath).ClientConn())

	runClient, commandID, cancel := startCommandOnDaemon(t, client,
		"echo up && read line",
		graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP,
	)

	recvUntilStdoutContains(t, runClient, "up\n")

	// Detach first so the exit status has no attached stream to deliver to.
	cancel()

	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 1 && !infos[0].GetAttached()
	})

	_, err := client.KillCommand(t.Context(), &graftv1.KillCommandRequest{CommandId: commandID})
	test.That(t, err, test.ShouldBeNil)

	// The keep-policy command stays listed as exited (status undelivered)...
	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 1 && !infos[0].GetRunning()
	})

	// ...and killing an exited command forgets it.
	_, err = client.KillCommand(t.Context(), &graftv1.KillCommandRequest{CommandId: commandID})
	test.That(t, err, test.ShouldBeNil)

	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 0
	})

	_, err = client.KillCommand(t.Context(), &graftv1.KillCommandRequest{CommandId: commandID})
	test.That(t, status.Code(err), test.ShouldEqual, codes.NotFound)
}

func TestDetachCommandRPC(t *testing.T) {
	sockPath, srv := startTestRemoteDaemonWithServer(t)
	srv.cmdRegistry.detachKillDelay = 100 * time.Millisecond
	client := graftv1.NewGraftServiceClient(connectToTestDaemon(t, sockPath).ClientConn())

	runClient, commandID, cancel := startCommandOnDaemon(t, client,
		"echo up && read line",
		graftv1.CommandPersistence_COMMAND_PERSISTENCE_KILL,
	)
	defer cancel()

	recvUntilStdoutContains(t, runClient, "up\n")

	resp, err := client.DetachCommand(t.Context(), &graftv1.DetachCommandRequest{CommandId: commandID})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp.GetWasAttached(), test.ShouldBeTrue)

	// The booted client's stream ends with Aborted.
	for {
		_, recvErr := runClient.Recv()
		if recvErr != nil {
			test.That(t, status.Code(recvErr), test.ShouldEqual, codes.Aborted)

			break
		}
	}

	// Well past the old kill-policy delay the command still runs: an explicit
	// detach flips it to keep.
	waitForCommandState(t, client, func(infos []*graftv1.CommandInfo) bool {
		return len(infos) == 1 && infos[0].GetRunning() && !infos[0].GetAttached() &&
			infos[0].GetPersistence() == graftv1.CommandPersistence_COMMAND_PERSISTENCE_KEEP
	})

	select {
	case <-t.Context().Done():
		return
	case <-time.After(400 * time.Millisecond):
	}

	infos := listCommandsOnDaemon(t, client)
	test.That(t, len(infos), test.ShouldEqual, 1)
	test.That(t, infos[0].GetRunning(), test.ShouldBeTrue)

	// Cleanup.
	_, err = client.KillCommand(t.Context(), &graftv1.KillCommandRequest{CommandId: commandID, Signal: "SIGKILL"})
	test.That(t, err, test.ShouldBeNil)
}

// TestGroupConnectionsByDaemonDedupesSharedDaemon covers connections
// restored under two different names (e.g. two SSH aliases resolving to the
// same host) that end up sharing a single underlying remoteDaemon. They must
// collapse into one group so `graft ps` doesn't list the same command twice.
func TestGroupConnectionsByDaemonDedupesSharedDaemon(t *testing.T) {
	sharedDaemon := &remoteDaemon{}
	otherDaemon := &remoteDaemon{}

	conns := map[string]*Connection{
		"dev-a":   newConnection(sharedDaemon, "dev-a", "", "", false),
		"dev-b":   newConnection(sharedDaemon, "dev-b", "", "", false),
		"staging": newConnection(otherDaemon, "staging", "", "", false),
	}

	groups := groupConnectionsByDaemon(conns, "")
	test.That(t, len(groups), test.ShouldEqual, 2)

	byNames := map[string][]string{}
	for _, g := range groups {
		byNames[strings.Join(g.names, ",")] = g.names
	}

	test.That(t, byNames["dev-a,dev-b"], test.ShouldResemble, []string{"dev-a", "dev-b"})
	test.That(t, byNames["staging"], test.ShouldResemble, []string{"staging"})
}

// TestGroupConnectionsByDaemonFiltersByName covers the --to case: only the
// named connection's group should come back, even if it shares a daemon with
// others.
func TestGroupConnectionsByDaemonFiltersByName(t *testing.T) {
	sharedDaemon := &remoteDaemon{}

	conns := map[string]*Connection{
		"dev-a": newConnection(sharedDaemon, "dev-a", "", "", false),
		"dev-b": newConnection(sharedDaemon, "dev-b", "", "", false),
	}

	groups := groupConnectionsByDaemon(conns, "dev-b")
	test.That(t, len(groups), test.ShouldEqual, 1)
	test.That(t, groups[0].names, test.ShouldResemble, []string{"dev-b"})
}

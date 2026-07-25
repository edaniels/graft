package graft

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// ListManagedCommands returns managed commands across all connections, or a
// single one when connectionName is set.
func (client *LocalClient) ListManagedCommands(
	ctx context.Context,
	connectionName string,
) ([]*graftv1.CommandInfo, error) {
	resp, err := client.ListCommands(ctx, &graftv1.ListCommandsRequest{
		ConnectionName: connectionName,
	})
	if err != nil {
		return nil, client.handleError(err)
	}

	return resp.GetCommands(), nil
}

// PrintManagedCommands renders the `graft ps` table.
func (client *LocalClient) PrintManagedCommands(ctx context.Context, connectionName string) error {
	commands, err := client.ListManagedCommands(ctx, connectionName)
	if err != nil {
		return err
	}

	if len(commands) == 0 {
		fmt.Fprintln(client.outWriter, "No managed commands 😎")

		return nil
	}

	slices.SortFunc(commands, func(a, b *graftv1.CommandInfo) int {
		if c := strings.Compare(a.GetConnectionName(), b.GetConnectionName()); c != 0 {
			return c
		}

		if a.GetStartedAtUnix() != b.GetStartedAtUnix() {
			return int(a.GetStartedAtUnix() - b.GetStartedAtUnix())
		}

		return strings.Compare(a.GetCommandId(), b.GetCommandId())
	})

	writer := tabwriter.NewWriter(client.outWriter, 2, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tCONNECTION\tSTATE\tSTARTED\tCOMMAND")

	for _, info := range commands {
		state := "detached"

		switch {
		case !info.GetRunning():
			state = fmt.Sprintf("exited(%d)", info.GetExitStatus())
		case info.GetAttached():
			state = "attached"
		}

		started := time.Unix(info.GetStartedAtUnix(), 0).Format(time.DateTime)

		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			info.GetCommandId(), info.GetConnectionName(), state, started,
			sanitizeTableCell(info.GetCommand()))
	}

	return errors.Wrap(writer.Flush())
}

// sanitizeTableCell keeps command lines from corrupting the tab-aligned table.
func sanitizeTableCell(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return ' '
		default:
			return r
		}
	}, s)
}

// KillManagedCommand signals a managed command's process group. An empty
// signal means SIGTERM; an empty connectionName searches all connections.
func (client *LocalClient) KillManagedCommand(ctx context.Context, commandID, connectionName, signal string) error {
	resp, err := client.KillCommand(ctx, &graftv1.KillCommandRequest{
		CommandId:      commandID,
		ConnectionName: connectionName,
		Signal:         signal,
	})
	if err != nil {
		return client.handleError(err)
	}

	if resp.GetWasExited() {
		fmt.Fprintf(client.outWriter, "already exited (status %d)\n", resp.GetExitStatus())
	}

	return nil
}

// AttachManagedCommand re-attaches the terminal to a managed command,
// replaying its buffered output, and streams it until the command exits or
// the attachment ends (`graft detach`, or another client attaching). Signals
// received while attached are forwarded to the command.
func (client *LocalClient) AttachManagedCommand(
	ctx context.Context,
	commandID, connectionName string,
) (int, error) {
	// The stream must not die on Ctrl-C: SIGINT is forwarded to the attached
	// command (see Handle), so the CLI's signal-canceled context cannot be
	// the stream's lifetime.
	runClient, err := client.GraftServiceClient.RunCommand(context.WithoutCancel(ctx))
	if err != nil {
		return 0, client.handleError(err)
	}

	if sendErr := runClient.Send(&graftv1.RunCommandRequest{
		Data: &graftv1.RunCommandRequest_Attach{
			Attach: &graftv1.AttachCommand{
				CommandId:      commandID,
				ConnectionName: connectionName,
			},
		},
	}); sendErr != nil {
		return 0, errors.Wrap(sendErr)
	}

	resp, err := runClient.Recv()
	if err != nil {
		return 0, client.handleError(err)
	}

	attached, ok := resp.GetData().(*graftv1.RunCommandResponse_Attached)
	if !ok {
		return 0, errors.New("expected CommandAttached response from server")
	}

	runningCmd := NewRemoteRunningCommand(runClient)

	interactive := attached.Attached.GetPty() && term.IsTerminal(int(os.Stdin.Fd()))
	if interactive {
		stdinFd := int(os.Stdin.Fd())

		oldState, rawErr := term.MakeRaw(stdinFd)
		if rawErr != nil {
			return 0, errors.WrapPrefix(rawErr, "error entering raw mode")
		}

		defer func() {
			if restoreErr := term.Restore(stdinFd, oldState); restoreErr != nil {
				slog.ErrorContext(ctx, "error restoring terminal state", "error", restoreErr)
			}
		}()
	}

	handler := RunCommandGRPCClientHandler{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		runningCmd: runningCmd,
	}

	code, handleErr := handler.Handle(ctx)
	if handleErr != nil && status.Code(handleErr) == codes.Aborted {
		// The attachment was taken away (graft detach, or another client
		// attached); the command itself keeps running. Raw mode may still be
		// active, hence \r\n.
		fmt.Fprint(os.Stderr, "\r\n[detached]\r\n")

		return 0, nil
	}

	return code, handleErr
}

// DetachManagedCommand disconnects whatever client is attached to a managed
// command, leaving the command running and re-attachable.
func (client *LocalClient) DetachManagedCommand(ctx context.Context, commandID, connectionName string) error {
	resp, err := client.DetachCommand(ctx, &graftv1.DetachCommandRequest{
		CommandId:      commandID,
		ConnectionName: connectionName,
	})
	if err != nil {
		return client.handleError(err)
	}

	if !resp.GetWasAttached() {
		fmt.Fprintln(client.errWriter, "no client attached")
	}

	return nil
}

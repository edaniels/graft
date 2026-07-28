package graft

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"

	"github.com/edaniels/graft/errors"
	graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"
)

// claudeSessionStartHookOutput is the subset of Claude Code's SessionStart
// hook output contract used to inject additional context at session start.
type claudeSessionStartHookOutput struct {
	HookSpecificOutput claudeSessionStartHookSpecificOutput `json:"hookSpecificOutput"`
}

type claudeSessionStartHookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// PrintClaudeSessionStartHook writes a Claude Code SessionStart hook payload
// describing graft's resolved connection, shimmed commands, and port
// forwards for the current directory. It never returns an error and prints
// nothing when the current directory has no eligible graft connection,
// since that is the overwhelmingly common case for a hook wired into every
// project regardless of whether graft applies there.
func (client *LocalClient) PrintClaudeSessionStartHook(ctx context.Context) error {
	additionalContext, err := client.claudeSessionStartContext(ctx)
	if err != nil || additionalContext == "" {
		//nolint:nilerr // a hook should stay silent, not fail the session, on any lookup error
		return nil
	}

	data, err := json.Marshal(claudeSessionStartHookOutput{
		HookSpecificOutput: claudeSessionStartHookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: additionalContext,
		},
	})
	if err != nil {
		//nolint:nilerr // a hook should stay silent, not fail the session, on any lookup error
		return nil
	}

	fmt.Fprintln(client.outWriter, string(data))

	return nil
}

// claudeSessionStartContext resolves the connection for the current
// directory and formats its shimmed commands and port forwards as text
// for injection into a Claude Code session's context. It returns an empty
// string, not an error, when the current directory has no eligible graft
// connection.
func (client *LocalClient) claudeSessionStartContext(ctx context.Context) (string, error) {
	selectResp, err := client.selectConnection(ctx)
	if err != nil {
		if hint := connectionNameHint(err); hint != "" {
			return formatConnectionHintContext(hint), nil
		}

		// The only other error selectConnection produces is "no eligible
		// connection for this directory", which is true of most directories
		// and not worth surfacing as a failure.

		return "", nil
	}

	connName := selectResp.GetConnectionName()

	shimResp, err := client.SessionShimmedCommands(ctx, &graftv1.SessionShimmedCommandsRequest{
		Pid: client.ppid,
		Cwd: client.cwd,
	})
	if err != nil {
		return "", errors.Wrap(err)
	}

	listResp, err := client.ListConnections(ctx, &graftv1.ListConnectionsRequest{Pid: client.ppid})
	if err != nil {
		return "", errors.Wrap(err)
	}

	return formatClaudeSessionStartContext(connName, listResp.GetConnections()[connName], shimResp.GetDestinationCommands()), nil
}

// connectionNameHint extracts the connectionNameHint ErrorInfo metadata from
// err, if present, or "" otherwise.
func connectionNameHint(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			if hint, ok := info.GetMetadata()[errors.ErrorMetadataFieldConnectionNameHint]; ok {
				return hint
			}
		}
	}

	return ""
}

// formatConnectionHintContext formats context for the case where no
// connection is bound to the current directory but exactly one connection
// exists and could be used explicitly.
func formatConnectionHintContext(hint string) string {
	return fmt.Sprintf(
		"graft: no connection is bound to this directory, but connection %q is configured. "+
			"Run commands on it explicitly with `graft run --to %s <command> [args...]`, "+
			"or pin it to this directory going forward with `graft use %s`.",
		hint, hint, hint)
}

// formatClaudeSessionStartContext formats a connection's resolved state,
// shimmed commands, and port forwards as text for a Claude Code
// SessionStart hook's additional context.
func formatClaudeSessionStartContext(
	connName string,
	cs *graftv1.ConnectionStatus,
	destCommands map[string]*graftv1.CommandForwardings,
) string {
	var buf strings.Builder

	fmt.Fprintf(&buf, "graft: this directory is connected to %q (%s) [%s]",
		connName, cs.GetSafeDestination(), claudeConnectionStateText(cs.GetState()))

	if reason := cs.GetStateReason(); reason != "" {
		fmt.Fprintf(&buf, ": %s", reason)
	}

	buf.WriteString(".\n")

	var active, inactive []string

	for _, fwds := range destCommands {
		for _, cmd := range fwds.GetCommands() {
			if cmd.GetActive() {
				active = append(active, cmd.GetLocal())
			} else {
				inactive = append(inactive, cmd.GetLocal())
			}
		}
	}

	sort.Strings(active)
	sort.Strings(inactive)

	if len(active) > 0 {
		fmt.Fprintf(&buf, "graft can run these commands on %q instead of locally: %s.\n",
			connName, strings.Join(active, ", "))
	}

	if len(inactive) > 0 {
		fmt.Fprintf(&buf, "These commands are configured to forward to %q but are not currently reachable there: %s.\n",
			connName, strings.Join(inactive, ", "))
	}

	buf.WriteString(
		"Note: the commands above only forward once this session's Bash shell has run " +
			"`eval \"$(graft activate bash)\"` (or the zsh/fish equivalent); Claude Code's Bash tool keeps one " +
			"shell alive for the rest of the session, so run activation once if it hasn't happened yet and it " +
			"will stay in effect. Until then, plain commands execute locally instead of on this connection. " +
			"To target this connection explicitly regardless of activation state, use " +
			fmt.Sprintf("`graft run --to %s <command> [args...]`", connName) +
			". Once activated, `sudo <cmd>` is also unconditionally forwarded if <cmd> is shimmed, executing " +
			"remotely as root with no extra confirmation.\n")

	var ports []string

	for _, pf := range cs.GetPortForwardStatuses() {
		if pf.GetConflict() {
			ports = append(ports, fmt.Sprintf("CONFLICT %s %d: %s", pf.GetProtocol(), pf.GetRemotePort(), pf.GetConflictReason()))
		} else {
			ports = append(ports, fmt.Sprintf("%s localhost:%d -> remote:%d",
				pf.GetProtocol(), pf.GetLocalPort(), pf.GetRemotePort()))
		}
	}

	if len(ports) > 0 {
		fmt.Fprintf(&buf, "Port forwards: %s.\n", strings.Join(ports, "; "))
	}

	return buf.String()
}

// claudeConnectionStateText returns a plain-text (no ANSI color) rendering
// of a connection state, suitable for injection into a Claude Code hook's
// additional context rather than a terminal.
func claudeConnectionStateText(state graftv1.ConnectionState) string {
	switch state { //nolint:exhaustive
	case graftv1.ConnectionState_CONNECTION_STATE_INITIALIZING:
		return "initializing"
	case graftv1.ConnectionState_CONNECTION_STATE_CONNECTED:
		return "connected"
	case graftv1.ConnectionState_CONNECTION_STATE_FAILED:
		return "failed"
	case graftv1.ConnectionState_CONNECTION_STATE_CLOSED:
		return "closed"
	case graftv1.ConnectionState_CONNECTION_STATE_RECONNECTING:
		return "reconnecting"
	default:
		return "unknown"
	}
}

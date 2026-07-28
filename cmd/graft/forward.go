package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	graft "github.com/edaniels/graft/pkg"
)

var (
	// forwardTo is the persistent --to/-t flag shared by `forward` and all
	// `forward <subcommand>` commands. PersistentFlags on the parent makes
	// it inherited and settable in any position on the command line.
	forwardTo     string
	forwardPrefix bool

	// forwardAgentFlag and forwardRemoveAgentFlag are deliberately flags, not
	// a subcommand: a subcommand named "agent" would silently shadow forwarding
	// a real command or port literally named "agent" (there are real tools with
	// that name, e.g. various *-agent daemons). A flag can never collide with a
	// positional command/port argument, so this ambiguity can't exist by
	// construction rather than needing a --/escape-hatch convention remembered
	// case by case.
	forwardAgentFlag       bool
	forwardRemoveAgentFlag bool
)

func partitionForwardArgs(args []string) ([]string, []string) {
	var commands, ports []string

	for _, arg := range args {
		if graft.IsPortSpec(arg) {
			ports = append(ports, arg)
		} else {
			commands = append(commands, arg)
		}
	}

	return commands, ports
}

var forwardCmd = &cobra.Command{
	Use:   "forward [flags] [command|port] [commands|ports...]",
	Short: "Forward local commands, ports, or the SSH agent to a remote connection",
	Long: `Forward local commands, ports, or the SSH agent to a remote connection.

Arguments that look like port specs (e.g. 8080, 3000:8080, 5432/tcp) are
forwarded as ports. All other arguments are forwarded as commands.

Port spec format: [local_port:]remote_port[/protocol]
  8080           Forward remote port 8080 to local 8080 (tcp)
  3000:8080      Forward remote port 8080 to local 3000
  5432/tcp       Explicit protocol
  5353/udp       UDP forward
  3000:8080/udp  Full form

Use --agent to keep the local SSH agent forwarded instead (takes no command/port arguments).`,
	Args: func(cmd *cobra.Command, args []string) error {
		if forwardAgentFlag {
			return cobra.NoArgs(cmd, args)
		}

		return cobra.MinimumNArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		if forwardAgentFlag {
			return client.SetForwardAgent(ctx, true, toConn)
		}

		commands, ports := partitionForwardArgs(args)

		if len(commands) > 0 {
			if err := client.ForwardCommands(ctx, commands, toConn, forwardPrefix); err != nil {
				return err //nolint:wrapcheck
			}
		}

		if len(ports) > 0 {
			if err := client.AddPortForwards(ctx, ports, toConn); err != nil {
				return err //nolint:wrapcheck
			}
		}

		return nil
	},
}

var forwardListCmd = &cobra.Command{
	Use:   "list",
	Short: "List forwarded commands and ports",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			if selectResp, err := client.SelectConnectionForCWD(ctx); err == nil {
				toConn = selectResp.GetConnectionName()
			}
		}

		if err := client.PrintShimmedCommands(ctx); err != nil {
			return err //nolint:wrapcheck
		}

		return client.PrintPortForwards(ctx, toConn)
	},
}

var forwardRemoveCmd = &cobra.Command{
	Use:   "remove [flags] [command|port] [commands|ports...]",
	Short: "Remove forwarded commands, ports, or the SSH agent from a connection",
	Args: func(cmd *cobra.Command, args []string) error {
		if forwardRemoveAgentFlag {
			return cobra.NoArgs(cmd, args)
		}

		return cobra.MinimumNArgs(1)(cmd, args)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit(cmd, args, "--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
		}

		if forwardRemoveAgentFlag {
			return client.SetForwardAgent(ctx, false, toConn)
		}

		commands, ports := partitionForwardArgs(args)

		if len(commands) > 0 {
			if err := client.RemoveForwardCommands(ctx, commands, toConn); err != nil {
				return err //nolint:wrapcheck
			}
		}

		if len(ports) > 0 {
			autoDetected, err := client.RemovePortForwards(ctx, ports, toConn)
			if err != nil {
				return err //nolint:wrapcheck
			}

			for _, p := range autoDetected {
				fmt.Fprintf(os.Stderr, "warning: port %s is auto-detected, not explicitly forwarded"+
					"; it will stop being forwarded when the remote process stops listening on it\n", p.String())
			}
		}

		return nil
	},
}

var forwardWhichCmd = &cobra.Command{
	Use:   "which <command>",
	Short: "Show which connection a forwarded command uses",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if graft.IsPortSpec(args[0]) {
			return cliExit(cmd, args, "port forwards are connection-wide; use 'graft forward list' to see them", 1)
		}

		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		return client.Which(ctx, args[0])
	},
}

func init() {
	// PersistentFlags so --to/-t is inherited by all forward subcommands and
	// can be set in any position on the command line.
	forwardCmd.PersistentFlags().StringVarP(&forwardTo, "to", "t", "", "Target connection (detected from CWD if omitted)")
	forwardCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck
	forwardCmd.Flags().BoolVar(&forwardPrefix, "prefix", false, "Forward with connection name prefix")
	forwardCmd.Flags().BoolVar(&forwardAgentFlag, "agent", false, "Keep the local SSH agent forwarded instead of a command/port")
	forwardRemoveCmd.Flags().BoolVar(&forwardRemoveAgentFlag, "agent", false, "Stop forwarding the local SSH agent instead of a command/port")

	forwardCmd.AddCommand(forwardListCmd)
	forwardCmd.AddCommand(forwardRemoveCmd)
	forwardCmd.AddCommand(forwardWhichCmd)

	rootCmd.AddCommand(forwardCmd)
}

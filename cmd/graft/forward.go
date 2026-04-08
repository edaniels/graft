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
	Use:   "forward [flags] <command|port> [commands|ports...]",
	Short: "Forward local commands or ports to a remote connection",
	Long: `Forward local commands or ports to a remote connection.

Arguments that look like port specs (e.g. 8080, 3000:8080, 5432/tcp) are
forwarded as ports. All other arguments are forwarded as commands.

Port spec format: [local_port:]remote_port[/protocol]
  8080           Forward remote port 8080 to local 8080 (tcp)
  3000:8080      Forward remote port 8080 to local 3000
  5432/tcp       Explicit protocol
  5353/udp       UDP forward
  3000:8080/udp  Full form`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit("--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
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
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
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
	Use:   "remove [flags] <command|port> [commands|ports...]",
	Short: "Remove forwarded commands or ports from a connection",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		toConn := forwardTo
		if toConn == "" {
			selectResp, err := client.SelectConnectionForCWD(ctx)
			if err != nil {
				return cliExit("--to required (no connection detected for current directory)", 1)
			}

			toConn = selectResp.GetConnectionName()
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
			return cliExit("port forwards are connection-wide; use 'graft forward list' to see them", 1)
		}

		client, ctx := newClient(cmd.Context(), true)
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

	forwardCmd.AddCommand(forwardListCmd)
	forwardCmd.AddCommand(forwardRemoveCmd)
	forwardCmd.AddCommand(forwardWhichCmd)

	rootCmd.AddCommand(forwardCmd)
}

package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/edaniels/graft/errors"
)

var connectionCmd = &cobra.Command{
	Use:   "connection",
	Short: "Manage connections",
}

var connectionSetRootCmd = &cobra.Command{
	Use:   "set-root <connection> <local_dir> [remote_dir]",
	Short: "Set the local (and optionally remote) root directory for a connection",
	Args:  cobra.RangeArgs(2, 3),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeConnectionNames(cmd, args, toComplete)
		}

		return nil, cobra.ShellCompDirectiveDefault
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		connName := args[0]
		localDir := args[1]

		absDir, err := filepath.Abs(localDir)
		if err != nil {
			return errors.Wrap(err)
		}

		if _, statErr := os.Stat(absDir); statErr != nil {
			return errors.Errorf("local directory does not exist: %s", absDir)
		}

		var remoteDir string
		if len(args) == 3 {
			remoteDir = args[2]
		}

		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		if setErr := client.SetConnectionRoots(ctx, connName, absDir, remoteDir); setErr != nil {
			return errors.Wrap(setErr)
		}

		return nil
	},
}

var connCmdShellTo string

var connectionCommandsCmd = &cobra.Command{
	Use:   "available-commands",
	Short: "Print available commands; helpful for debugging shim availability",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		if err := client.PrintConnectionAvailableCommands(ctx, connCmdShellTo); err != nil {
			return errors.Wrap(err)
		}

		return nil
	},
}

func init() {
	// PersistentFlags so the flag is inherited by all subcommands and can be
	// set anywhere on the command line (before or after the subcommand name).
	connectionCmd.PersistentFlags().StringVarP(&connCmdShellTo, "to", "t", "", "Target connection")
	connectionCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	connectionCmd.AddCommand(connectionSetRootCmd)
	connectionCmd.AddCommand(connectionCommandsCmd)
	rootCmd.AddCommand(connectionCmd)
}

package main

import (
	"github.com/spf13/cobra"
)

var psTo string

var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "List commands managed by remote daemons",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, false)
		defer client.Close()

		if err := client.PrintManagedCommands(ctx, psTo); err != nil {
			return cliExit(cmd, args, err, 1)
		}

		return nil
	},
}

var attachTo string

var attachCmd = &cobra.Command{
	Use:   "attach <command-id>",
	Short: "Re-attach to a managed command, replaying recent output",
	Long: "Re-attach to a managed command, replaying recent output.\n\n" +
		"Signals sent to graft while attached (Ctrl-C, SIGTERM, SIGQUIT,\n" +
		"SIGUSR1/2) are forwarded to the command, like a local foreground\n" +
		"process. Detach without stopping the command by running\n" +
		"`graft detach <command-id>` from another terminal; a detached\n" +
		"command is kept alive regardless of its previous policy.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, false)
		defer client.Close()

		exitCode, err := client.AttachManagedCommand(ctx, args[0], attachTo)
		if err != nil {
			return cliExit(cmd, args, err, 1)
		}

		return cliExit(cmd, args, "", exitCode)
	},
}

var detachTo string

var detachCmd = &cobra.Command{
	Use:   "detach <command-id>",
	Short: "Disconnect a command's attached client, leaving it running",
	Long: "Disconnect whatever client is attached to a managed command. The\n" +
		"command keeps running (it is flipped to keep-alive) and can be\n" +
		"re-attached with `graft attach`.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, false)
		defer client.Close()

		if err := client.DetachManagedCommand(ctx, args[0], detachTo); err != nil {
			return cliExit(cmd, args, err, 1)
		}

		return nil
	},
}

var (
	killTo     string
	killSignal string
)

var killCmd = &cobra.Command{
	Use:   "kill <command-id>",
	Short: "Signal a managed command's process group",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, false)
		defer client.Close()

		if err := client.KillManagedCommand(ctx, args[0], killTo, killSignal); err != nil {
			return cliExit(cmd, args, err, 1)
		}

		return nil
	},
}

func init() {
	psCmd.Flags().StringVarP(&psTo, "to", "t", "", "Only list commands on this connection")
	psCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	attachCmd.Flags().StringVarP(&attachTo, "to", "t", "", "Connection the command runs on (searched if unset)")
	attachCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	detachCmd.Flags().StringVarP(&detachTo, "to", "t", "", "Connection the command runs on (searched if unset)")
	detachCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck

	killCmd.Flags().StringVarP(&killTo, "to", "t", "", "Connection the command runs on (searched if unset)")
	killCmd.RegisterFlagCompletionFunc("to", completeConnectionNames) //nolint:errcheck
	killCmd.Flags().StringVarP(&killSignal, "signal", "s", "", "Signal name to send (default SIGTERM)")

	rootCmd.AddCommand(psCmd)
	rootCmd.AddCommand(attachCmd)
	rootCmd.AddCommand(detachCmd)
	rootCmd.AddCommand(killCmd)
}

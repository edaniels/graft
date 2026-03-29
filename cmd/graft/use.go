package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var useClear bool

var useCmd = &cobra.Command{
	Use:   "use [connection-name]",
	Short: "Pin a connection to the current session",
	Long: `Pin a connection to the current shell session, overriding CWD-based auto-selection.

Use --clear to remove the pin and resume CWD-based selection.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeConnectionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && !useClear {
			return cliExit("connection name required (or use --clear to unpin)", 1)
		}

		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		var connName string
		if len(args) == 1 && !useClear {
			connName = args[0]
		}

		if err := client.PinConnection(ctx, connName); err != nil {
			return cliExit(err, 1)
		}

		if connName != "" {
			fmt.Fprintf(os.Stderr, "pinned session to %s\n", connName)
		} else {
			fmt.Fprintln(os.Stderr, "cleared session pin")
		}

		return nil
	},
}

func init() {
	useCmd.Flags().BoolVar(&useClear, "clear", false, "Clear the session pin")

	rootCmd.AddCommand(useCmd)
}

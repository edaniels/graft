package main

import "github.com/spf13/cobra"

var logsCmd = &cobra.Command{
	Use:   "logs <connection>",
	Short: "Export connection logs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.DumpLogs(ctx, args[0])
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
}

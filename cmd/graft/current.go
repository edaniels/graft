package main

import "github.com/spf13/cobra"

var currentCmd = &cobra.Command{
	Use:   "current",
	Short: "Print current connection for this session",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, ctx := newClient(cmd.Context(), cmd, args, true)
		defer client.Close()

		return client.SelectConnection(ctx)
	},
}

func init() {
	rootCmd.AddCommand(currentCmd)
}

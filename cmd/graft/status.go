package main

import "github.com/spf13/cobra"

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show connection status overview",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.PrintStatus(ctx)
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

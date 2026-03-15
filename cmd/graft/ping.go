package main

import "github.com/spf13/cobra"

var pingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Ping the graft daemon",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		return client.Ping(ctx)
	},
}

func init() {
	rootCmd.AddCommand(pingCmd)
}

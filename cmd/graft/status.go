package main

import "github.com/spf13/cobra"

var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show connection status overview",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		if statusJSON {
			return client.PrintStatusJSON(ctx)
		}

		return client.PrintStatus(ctx)
	},
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output status as JSON")

	rootCmd.AddCommand(statusCmd)
}

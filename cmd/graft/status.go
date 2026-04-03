package main

import "github.com/spf13/cobra"

var (
	statusJSON  bool
	statusWatch bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show connection status overview",
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, ctx := newClient(cmd.Context(), true)
		defer client.Close()

		if statusWatch {
			if statusJSON {
				return client.Watch(ctx, client.GetStatusJSON)
			}

			return client.Watch(ctx, client.GetStatus)
		}

		if statusJSON {
			return client.PrintStatusJSON(ctx)
		}

		return client.PrintStatus(ctx)
	},
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output status as JSON")
	statusCmd.Flags().BoolVarP(&statusWatch, "watch", "w", false, "Watch status for changes")

	rootCmd.AddCommand(statusCmd)
}

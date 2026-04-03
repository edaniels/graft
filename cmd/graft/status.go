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
		client, ctx := newClient(cmd.Context(), false)
		defer client.Close()

		printFn := client.PrintStatus
		if statusJSON {
			printFn = client.PrintStatusJSON
		}

		if statusWatch {
			return client.Watch(ctx, printFn)
		}

		return printFn(ctx)
	},
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output status as JSON")
	statusCmd.Flags().BoolVarP(&statusWatch, "watch", "w", false, "Watch status for changes")

	rootCmd.AddCommand(statusCmd)
}
